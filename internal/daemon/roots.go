package daemon

import (
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
)

const rootsTTL = 2 * time.Second

type rootSet struct {
	static  []string
	dir     string
	mu      sync.Mutex
	checked time.Time
	extra   []string
}

func newRootSet(static []string, dir string) *rootSet { return &rootSet{static: static, dir: dir} }

func (r *rootSet) All() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > rootsTTL {
		r.checked = time.Now()
		r.extra = config.DropInRoots(r.dir)
	}
	return append(append([]string(nil), r.static...), r.extra...)
}
