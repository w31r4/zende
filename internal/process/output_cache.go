package process

import (
	"container/list"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"
)

type cacheKey struct {
	Path           string
	Offset         int64
	Length         int
	Size, Modified int64
	Device, Inode  uint64
	Changed        int64
}

func outputKey(path string, offset int64, length int, st os.FileInfo) cacheKey {
	k := cacheKey{Path: path, Offset: offset, Length: length, Size: st.Size(), Modified: st.ModTime().UnixNano()}
	if raw, ok := st.Sys().(*syscall.Stat_t); ok {
		k.Device, k.Inode = uint64(raw.Dev), raw.Ino
		k.Changed = raw.Ctim.Sec*1e9 + raw.Ctim.Nsec
	}
	return k
}

type cacheBlock struct {
	key  cacheKey
	data []byte
}
type outputCache struct {
	mu          sync.Mutex
	limit, used int
	lru         *list.List
	blocks      map[cacheKey]*list.Element
}

func newOutputCache(limit int) *outputCache {
	return &outputCache{limit: limit, lru: list.New(), blocks: map[cacheKey]*list.Element{}}
}
func (c *outputCache) get(key cacheKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.blocks[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(e)
	return append([]byte(nil), e.Value.(cacheBlock).data...), true
}
func (c *outputCache) put(key cacheKey, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) > c.limit {
		return
	}
	if old := c.blocks[key]; old != nil {
		c.remove(old)
	}
	for c.used+len(data) > c.limit || c.lru.Len() >= 256 {
		c.remove(c.lru.Back())
	}
	c.blocks[key] = c.lru.PushFront(cacheBlock{key: key, data: append([]byte(nil), data...)})
	c.used += len(data)
}
func (c *outputCache) remove(e *list.Element) {
	if e == nil {
		return
	}
	b := e.Value.(cacheBlock)
	c.used -= len(b.data)
	delete(c.blocks, b.key)
	c.lru.Remove(e)
}
func (c *outputCache) forgetPrefix(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.blocks {
		if strings.HasPrefix(key.Path, path) {
			c.remove(e)
		}
	}
}

// DisplayText is the ordinary log projection, not a terminal emulator. Preserve
// printable text, newlines and tabs; spell every other control byte visibly.
// Raw bytes remain available separately for an explicit terminal handoff.
func DisplayText(raw []byte) string {
	var out strings.Builder
	for len(raw) > 0 {
		r, n := utf8.DecodeRune(raw)
		if r == utf8.RuneError && n == 1 {
			fmt.Fprintf(&out, "\\x%02x", raw[0])
			raw = raw[1:]
			continue
		}
		raw = raw[n:]
		switch {
		case r == '\n' || r == '\t':
			out.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			if r < 256 {
				fmt.Fprintf(&out, "\\x%02x", r)
			} else {
				fmt.Fprintf(&out, "\\u%04x", r)
			}
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
