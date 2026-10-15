package cache_test

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/cache"
)

func TestCache_ExpiresEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		c, err := cache.New(1024, time.Second, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		c.Set("app", []byte(`{"model":"mock/echo"}`), cache.Entry{Body: []byte(`{"answer":1}`)})
		if _, ok := c.Get("app", []byte(`{"model":"mock/echo"}`)); !ok {
			t.Fatal("entry missed before TTL")
		}
		now = now.Add(time.Second)
		if _, ok := c.Get("app", []byte(`{"model":"mock/echo"}`)); ok {
			t.Fatal("entry hit after TTL")
		}
	})
}

func TestCache_EvictsLeastRecentlyUsedByBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := cache.New(1<<20, time.Hour, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{}`)
		c.Set("app", []byte("first"), cache.Entry{Body: []byte(strings.Repeat("1", 400<<10))})
		c.Set("app", []byte("second"), cache.Entry{Body: []byte(strings.Repeat("2", 400<<10))})
		if _, ok := c.Get("app", []byte("first")); !ok {
			t.Fatal("first entry missed before eviction")
		}
		c.Set("app", []byte("third"), cache.Entry{Body: []byte(strings.Repeat("3", 400<<10))})
		if _, ok := c.Get("app", []byte("first")); !ok {
			t.Fatal("recently used entry was evicted")
		}
		if _, ok := c.Get("app", []byte("second")); ok {
			t.Fatal("least recently used entry was not evicted")
		}
		if _, ok := c.Get("app", body); ok {
			t.Fatal("unknown body hit")
		}
	})
}

func TestCache_IsolatesAppsAndExactBodies(t *testing.T) {
	c, err := cache.New(1024, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	c.Set("one", []byte(`{"a":1,"b":2}`), cache.Entry{Body: []byte("answer")})
	if _, ok := c.Get("two", []byte(`{"a":1,"b":2}`)); ok {
		t.Fatal("entry was shared between apps")
	}
	if _, ok := c.Get("one", []byte(`{"b":2,"a":1}`)); ok {
		t.Fatal("reordered JSON body hit")
	}
}
