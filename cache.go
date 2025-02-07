// An LRU cached aimed at high concurrency
package ccache

import (
	"container/list"
	"hash/fnv"
	"sync/atomic"
	"time"
)

// The cache has a generic 'control' channel that is used to send
// messages to the worker. These are the messages that can be sent to it
type getDropped struct {
	res chan int
}

type getSize struct {
	res chan int64
}

type setMaxSize struct {
	size int64
	done chan struct{}
}

type clear struct {
	done chan struct{}
}

type syncWorker struct {
	done chan struct{}
}

type gc struct {
	done chan struct{}
}

type Cache struct {
	*Configuration
	list        *list.List
	size        int64
	buckets     []*bucket
	bucketMask  uint32
	deletables  chan *Item
	promotables chan *Item
	control     chan interface{}
}

// Create a new cache with the specified configuration
// See ccache.Configure() for creating a configuration
func New(config *Configuration) *Cache {
	c := &Cache{
		list:          list.New(),
		Configuration: config,
		bucketMask:    uint32(config.buckets) - 1,
		buckets:       make([]*bucket, config.buckets),
		control:       make(chan interface{}),
	}
	for i := 0; i < config.buckets; i++ {
		c.buckets[i] = &bucket{
			lookup: make(map[string]*Item),
		}
	}
	c.restart()
	return c
}

func (c *Cache) ItemCount() int {
	count := 0
	for _, b := range c.buckets {
		count += b.itemCount()
	}
	return count
}

func (c *Cache) DeletePrefix(prefix string) int {
	count := 0
	for _, b := range c.buckets {
		count += b.deletePrefix(prefix, c.deletables)
	}
	return count
}

// Deletes all items that the matches func evaluates to true.
func (c *Cache) DeleteFunc(matches func(key string, item *Item) bool) int {
	count := 0
	for _, b := range c.buckets {
		count += b.deleteFunc(matches, c.deletables)
	}
	return count
}

func (c *Cache) ForEachFunc(matches func(key string, item *Item) bool) {
	for _, b := range c.buckets {
		if !b.forEachFunc(matches) {
			break
		}
	}
}

// Get an item from the cache. Returns nil if the item wasn't found.
// This can return an expired item. Use item.Expired() to see if the item
// is expired and item.TTL() to see how long until the item expires (which
// will be negative for an already expired item).
func (c *Cache) Get(key string) *Item {
	item := c.bucket(key).get(key)
	if item == nil {
		return nil
	}
	if !item.Expired() {
		c.promote(item)
	}
	return item
}

// Used when the cache was created with the Track() configuration option.
// Avoid otherwise
func (c *Cache) TrackingGet(key string) TrackedItem {
	item := c.Get(key)
	if item == nil {
		return NilTracked
	}
	item.track()
	return item
}

// Used when the cache was created with the Track() configuration option.
// Sets the item, and returns a tracked reference to it.
func (c *Cache) TrackingSet(key string, value interface{}, duration time.Duration) TrackedItem {
	return c.set(key, value, duration, true)
}

// Set the value in the cache for the specified duration
func (c *Cache) Set(key string, value interface{}, duration time.Duration) {
	c.set(key, value, duration, false)
}

// Replace the value if it exists, does not set if it doesn't.
// Returns true if the item existed an was replaced, false otherwise.
// Replace does not reset item's TTL
func (c *Cache) Replace(key string, value interface{}) bool {
	item := c.bucket(key).get(key)
	if item == nil {
		return false
	}
	c.Set(key, value, item.TTL())
	return true
}

// Attempts to get the value from the cache and calles fetch on a miss (missing
// or stale item). If fetch returns an error, no value is cached and the error
// is returned back to the caller.
// Note that Fetch merely calls the public Get and Set functions. If you want
// a different Fetch behavior, such as thundering herd protection or returning
// expired items, implement it in your application.
func (c *Cache) Fetch(key string, duration time.Duration, fetch func() (interface{}, error)) (*Item, error) {
	item := c.Get(key)
	if item != nil && !item.Expired() {
		return item, nil
	}
	value, err := fetch()
	if err != nil {
		return nil, err
	}
	return c.set(key, value, duration, false), nil
}

func (c *Cache) FetchWithLock(key string, duration time.Duration, fetch func() (interface{}, error)) (*Item, error) {
	item := c.Get(key)
	if item != nil && !item.Expired() {
		return item, nil
	}
	c.bucket(key).Lock()
	value, err := fetch()
	c.bucket(key).Unlock()
	if err != nil {
		return nil, err
	}
	return c.set(key, value, duration, false), nil
}

// Remove the item from the cache, return true if the item was present, false otherwise.
func (c *Cache) Delete(key string) bool {
	item := c.bucket(key).delete(key)
	if item != nil {
		c.deletables <- item
		return true
	}
	return false
}

// Clears the cache
// This is a control command.
func (c *Cache) Clear() {
	done := make(chan struct{})
	c.control <- clear{done: done}
	<-done
}

// Stops the background worker. Operations performed on the cache after Stop
// is called are likely to panic
// This is a control command.
func (c *Cache) Stop() {
	close(c.promotables)
	<-c.control
}

// Gets the number of items removed from the cache due to memory pressure since
// the last time GetDropped was called
// This is a control command.
func (c *Cache) GetDropped() int {
	return doGetDropped(c.control)
}

func doGetDropped(controlCh chan<- interface{}) int {
	res := make(chan int)
	controlCh <- getDropped{res: res}
	return <-res
}

// SyncUpdates waits until the cache has finished asynchronous state updates for any operations
// that were done by the current goroutine up to now.
//
// For efficiency, the cache's implementation of LRU behavior is partly managed by a worker
// goroutine that updates its internal data structures asynchronously. This means that the
// cache's state in terms of (for instance) eviction of LRU items is only eventually consistent;
// there is no guarantee that it happens before a Get or Set call has returned. Most of the time
// application code will not care about this, but especially in a test scenario you may want to
// be able to know when the worker has caught up.
//
// This applies only to cache methods that were previously called by the same goroutine that is
// now calling SyncUpdates. If other goroutines are using the cache at the same time, there is
// no way to know whether any of them still have pending state updates when SyncUpdates returns.
// This is a control command.
func (c *Cache) SyncUpdates() {
	doSyncUpdates(c.control)
}

func doSyncUpdates(controlCh chan<- interface{}) {
	done := make(chan struct{})
	controlCh <- syncWorker{done: done}
	<-done
}

// Sets a new max size. That can result in a GC being run if the new maxium size
// is smaller than the cached size
// This is a control command.
func (c *Cache) SetMaxSize(size int64) {
	done := make(chan struct{})
	c.control <- setMaxSize{size: size, done: done}
	<-done
}

// Forces GC. There should be no reason to call this function, except from tests
// which require synchronous GC.
// This is a control command.
func (c *Cache) GC() {
	done := make(chan struct{})
	c.control <- gc{done: done}
	<-done
}

// Gets the size of the cache. This is an O(1) call to make, but it is handled
// by the worker goroutine. It's meant to be called periodically for metrics, or
// from tests.
// This is a control command.
func (c *Cache) GetSize() int64 {
	res := make(chan int64)
	c.control <- getSize{res}
	return <-res
}

func (c *Cache) restart() {
	c.deletables = make(chan *Item, c.deleteBuffer)
	c.promotables = make(chan *Item, c.promoteBuffer)
	c.control = make(chan interface{})
	go c.worker()
}

func (c *Cache) deleteItem(bucket *bucket, item *Item) {
	bucket.delete(item.key) //stop other GETs from getting it
	c.deletables <- item
}

func (c *Cache) set(key string, value interface{}, duration time.Duration, track bool) *Item {
	item, existing := c.bucket(key).set(key, value, duration, track)
	if existing != nil {
		c.deletables <- existing
	}
	c.promote(item)
	return item
}

func (c *Cache) bucket(key string) *bucket {
	h := fnv.New32a()
	h.Write([]byte(key))
	return c.buckets[h.Sum32()&c.bucketMask]
}

func (c *Cache) promote(item *Item) {
	select {
	case c.promotables <- item:
	default:
	}

}

func (c *Cache) worker() {
	defer close(c.control)
	dropped := 0
	promoteItem := func(item *Item) {
		if c.doPromote(item) && c.size > c.maxSize {
			dropped += c.gc()
		}
	}
	for {
		select {
		case item, ok := <-c.promotables:
			if ok == false {
				goto drain
			}
			promoteItem(item)
		case item := <-c.deletables:
			c.doDelete(item)
		case control := <-c.control:
			switch msg := control.(type) {
			case getDropped:
				msg.res <- dropped
				dropped = 0
			case setMaxSize:
				c.maxSize = msg.size
				if c.size > c.maxSize {
					dropped += c.gc()
				}
				msg.done <- struct{}{}
			case clear:
				for _, bucket := range c.buckets {
					bucket.clear()
				}
				c.size = 0
				c.list = list.New()
				msg.done <- struct{}{}
			case getSize:
				msg.res <- c.size
			case gc:
				dropped += c.gc()
				msg.done <- struct{}{}
			case syncWorker:
				doAllPendingPromotesAndDeletes(c.promotables, promoteItem,
					c.deletables, c.doDelete)
				msg.done <- struct{}{}
			}
		}
	}

drain:
	for {
		select {
		case item := <-c.deletables:
			c.doDelete(item)
		default:
			close(c.deletables)
			return
		}
	}
}

// This method is used to implement SyncUpdates. It simply receives and processes as many
// items as it can receive from the promotables and deletables channels immediately without
// blocking. If some other goroutine sends an item on either channel after this method has
// finished receiving, that's OK, because SyncUpdates only guarantees processing of values
// that were already sent by the same goroutine.
func doAllPendingPromotesAndDeletes(
	promotables <-chan *Item,
	promoteFn func(*Item),
	deletables <-chan *Item,
	deleteFn func(*Item),
) {
doAllPromotes:
	for {
		select {
		case item := <-promotables:
			if item != nil {
				promoteFn(item)
			}
		default:
			break doAllPromotes
		}
	}
doAllDeletes:
	for {
		select {
		case item := <-deletables:
			deleteFn(item)
		default:
			break doAllDeletes
		}
	}
}

func (c *Cache) doDelete(item *Item) {
	if item.element == nil {
		item.promotions = -2
	} else {
		c.size -= item.size
		if c.onDelete != nil {
			c.onDelete(item)
		}
		c.list.Remove(item.element)
	}
}

func (c *Cache) doPromote(item *Item) bool {
	//already deleted
	if item.promotions == -2 {
		return false
	}
	if item.element != nil { //not a new item
		if item.shouldPromote(c.getsPerPromote) {
			c.list.MoveToFront(item.element)
			item.promotions = 0
		}
		return false
	}

	c.size += item.size
	item.element = c.list.PushFront(item)
	return true
}

func (c *Cache) gc() int {
	dropped := 0
	element := c.list.Back()

	itemsToPrune := int64(c.itemsToPrune)
	if min := c.size - c.maxSize; min > itemsToPrune {
		itemsToPrune = min
	}

	for i := int64(0); i < itemsToPrune; i++ {
		if element == nil {
			return dropped
		}
		prev := element.Prev()
		item := element.Value.(*Item)
		if c.tracking == false || atomic.LoadInt32(&item.refCount) == 0 {
			c.bucket(item.key).delete(item.key)
			c.size -= item.size
			c.list.Remove(element)
			if c.onDelete != nil {
				c.onDelete(item)
			}
			dropped += 1
			item.promotions = -2
		}
		element = prev
	}
	return dropped
}
