package ccache

import (
	"container/list"
	"fmt"
	"sync/atomic"
	"time"
)

type Sized interface {
	Size() int64
}

type TrackedItem interface {
	Value() interface{}
	Release()
	Expired() bool
	TTL() time.Duration
	Expires() time.Time
	Extend(duration time.Duration)
}

type nilItem struct{}

func (n *nilItem) Value() interface{} { return nil }
func (n *nilItem) Release()           {}

func (i *nilItem) Expired() bool {
	return true
}

func (i *nilItem) TTL() time.Duration {
	return time.Minute
}

func (i *nilItem) Expires() time.Time {
	return time.Time{}
}

func (i *nilItem) Extend(duration time.Duration) {
}

var NilTracked = new(nilItem)

type Item struct {
	key        string
	group      string
	promotions int32
	refCount   int32
	expires    int64
	size       int64
	value      interface{}
	element    *list.Element
}

func newItem(key string, value interface{}, expires int64, track bool) *Item {
	size := int64(1)
	if sized, ok := value.(Sized); ok {
		size = sized.Size()
	}
	item := &Item{
		key:        key,
		value:      value,
		promotions: 0,
		size:       size,
		expires:    expires,
	}
	if track {
		item.refCount = 1
	}
	return item
}

func (i *Item) shouldPromote(getsPerPromote int32) bool {
	i.promotions += 1
	return i.promotions == getsPerPromote
}

func (i *Item) Value() interface{} {
	return i.value
}

func (i *Item) track() {
	atomic.AddInt32(&i.refCount, 1)
}

func (i *Item) Release() {
	atomic.AddInt32(&i.refCount, -1)
}

func (i *Item) Expired() bool {
	expires := atomic.LoadInt64(&i.expires)
	return expires < time.Now().UnixNano()
}

func (i *Item) TTL() time.Duration {
	expires := atomic.LoadInt64(&i.expires)
	return time.Nanosecond * time.Duration(expires-time.Now().UnixNano())
}

func (i *Item) Expires() time.Time {
	expires := atomic.LoadInt64(&i.expires)
	return time.Unix(0, expires)
}

func (i *Item) Extend(duration time.Duration) {
	atomic.StoreInt64(&i.expires, time.Now().Add(duration).UnixNano())
}

// String returns a string representation of the Item. This includes the default string
// representation of its Value(), as implemented by fmt.Sprintf with "%v", but the exact
// format of the string should not be relied on; it is provided only for debugging
// purposes, and because otherwise including an Item in a call to fmt.Printf or
// fmt.Sprintf expression could cause fields of the Item to be read in a non-thread-safe
// way.
func (i *Item) String() string {
	return fmt.Sprintf("Item(%v)", i.value)
}
