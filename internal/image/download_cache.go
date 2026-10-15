package image

import (
	"container/list"
	"fmt"
	"sync"
)

const defaultVerifiedImageCacheBytes = 512 * 1024 * 1024

type verifiedImageCacheEntry struct {
	key         string
	data        []byte
	contentType string
}

type verifiedImageByteCache struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	order    *list.List
	maxBytes int64
	curBytes int64
}

var verifiedImageCache = newVerifiedImageByteCache(0)

func newVerifiedImageByteCache(maxBytes int64) *verifiedImageByteCache {
	if maxBytes <= 0 {
		maxBytes = defaultVerifiedImageCacheBytes
	}
	return &verifiedImageByteCache{
		items:    make(map[string]*list.Element),
		order:    list.New(),
		maxBytes: maxBytes,
	}
}

func verifiedImageCacheKey(taskID string, idx int, ref string) string {
	return fmt.Sprintf("%s|%d|%s", taskID, idx, ref)
}

func PutVerifiedImage(taskID string, idx int, ref string, data []byte, contentType string) {
	verifiedImageCache.Put(verifiedImageCacheKey(taskID, idx, ref), data, contentType)
}

func GetVerifiedImage(taskID string, idx int, ref string) ([]byte, string, bool) {
	return verifiedImageCache.Get(verifiedImageCacheKey(taskID, idx, ref))
}

func (c *verifiedImageByteCache) Get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, "", false
	}
	c.order.MoveToBack(el)
	e := el.Value.(*verifiedImageCacheEntry)
	return e.data, e.contentType, true
}

func (c *verifiedImageByteCache) Put(key string, data []byte, contentType string) {
	if len(data) == 0 || int64(len(data)) > c.maxBytes {
		return
	}
	stored := slicesClone(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		old := el.Value.(*verifiedImageCacheEntry)
		c.curBytes -= int64(len(old.data))
		old.data = stored
		old.contentType = contentType
		c.curBytes += int64(len(stored))
		c.order.MoveToBack(el)
		return
	}
	e := &verifiedImageCacheEntry{key: key, data: stored, contentType: contentType}
	el := c.order.PushBack(e)
	c.items[key] = el
	c.curBytes += int64(len(stored))
	for c.curBytes > c.maxBytes {
		front := c.order.Front()
		if front == nil {
			break
		}
		old := front.Value.(*verifiedImageCacheEntry)
		c.order.Remove(front)
		delete(c.items, old.key)
		c.curBytes -= int64(len(old.data))
	}
}

func slicesClone(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	return out
}
