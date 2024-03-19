package bloomshipper

import (
	"context"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/loki/pkg/storage/stores/shipper/bloomshipper/config"
	"github.com/stretchr/testify/require"
)

var (
	logger = log.NewNopLogger()
)

func TestBlocksCache_ErrorCases(t *testing.T) {
	cfg := config.BlocksCacheConfig{
		TTL:       time.Hour,
		SoftLimit: flagext.Bytes(100),
		HardLimit: flagext.Bytes(200),
	}
	cache := NewFsBlocksCache(cfg, nil, logger)
	t.Cleanup(cache.Stop)

	t.Run("cancelled context", func(t *testing.T) {
		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)
		cancel()

		err := cache.Put(ctx, "key", BlockDirectory{})
		require.ErrorContains(t, err, "context canceled")

		err = cache.PutMany(ctx, []string{"key"}, []BlockDirectory{{}})
		require.ErrorContains(t, err, "context canceled")

		_, ok := cache.Get(ctx, "key")
		require.False(t, ok)
	})

	t.Run("duplicate keys", func(t *testing.T) {
		ctx := context.Background()

		err := cache.Put(ctx, "key", CacheValue("a", 10))
		require.NoError(t, err)

		err = cache.Put(ctx, "key", CacheValue("b", 10))
		require.ErrorContains(t, err, "entry already exists: key")
	})

	t.Run("multierror when putting many fails", func(t *testing.T) {
		ctx := context.Background()

		err := cache.PutMany(
			ctx,
			[]string{"x", "y", "x", "z"},
			[]BlockDirectory{
				CacheValue("x", 2),
				CacheValue("y", 2),
				CacheValue("x", 2),
				CacheValue("z", 250),
			},
		)
		require.ErrorContains(t, err, "2 errors: entry already exists: x; entry exceeds hard limit: z")
	})

	// TODO(chaudum): Implement blocking evictions
	t.Run("todo: blocking evictions", func(t *testing.T) {
		ctx := context.Background()

		err := cache.Put(ctx, "a", CacheValue("a", 5))
		require.NoError(t, err)

		err = cache.Put(ctx, "b", CacheValue("b", 10))
		require.NoError(t, err)

		err = cache.Put(ctx, "c", CacheValue("c", 190))
		require.Error(t, err, "todo: implement waiting for evictions to free up space")
	})
}

func CacheValue(path string, size int64) BlockDirectory {
	return BlockDirectory{
		Path: path,
		size: size,
	}
}

func TestBlocksCache_PutAndGet(t *testing.T) {
	cfg := config.BlocksCacheConfig{
		TTL:       time.Hour,
		SoftLimit: flagext.Bytes(10),
		HardLimit: flagext.Bytes(20),
		// no need for TTL evictions
		PurgeInterval: time.Minute,
	}
	cache := NewFsBlocksCache(cfg, nil, logger)
	t.Cleanup(cache.Stop)

	ctx := context.Background()
	err := cache.PutMany(
		ctx,
		[]string{"a", "b", "c"},
		[]BlockDirectory{CacheValue("a", 1), CacheValue("b", 2), CacheValue("c", 3)},
	)
	require.NoError(t, err)

	// key does not exist
	_, found := cache.Get(ctx, "d")
	require.False(t, found)

	// existing keys
	_, found = cache.Get(ctx, "b")
	require.True(t, found)
	_, found = cache.Get(ctx, "c")
	require.True(t, found)
	_, found = cache.Get(ctx, "a")
	require.True(t, found)

	require.Equal(t, 3, cache.lru.Len())

	// check LRU order
	elem := cache.lru.Front()
	require.Equal(t, "a", elem.Value.(*Entry).Key)
	require.Equal(t, int32(1), elem.Value.(*Entry).refCount.Load())

	elem = elem.Next()
	require.Equal(t, "c", elem.Value.(*Entry).Key)
	require.Equal(t, int32(1), elem.Value.(*Entry).refCount.Load())

	elem = elem.Next()
	require.Equal(t, "b", elem.Value.(*Entry).Key)
	require.Equal(t, int32(1), elem.Value.(*Entry).refCount.Load())

	// fetch more
	_, _ = cache.Get(ctx, "a")
	_, _ = cache.Get(ctx, "a")
	_, _ = cache.Get(ctx, "b")

	// LRU order changed
	elem = cache.lru.Front()
	require.Equal(t, "b", elem.Value.(*Entry).Key)
	require.Equal(t, int32(2), elem.Value.(*Entry).refCount.Load())

	elem = elem.Next()
	require.Equal(t, "a", elem.Value.(*Entry).Key)
	require.Equal(t, int32(3), elem.Value.(*Entry).refCount.Load())

	elem = elem.Next()
	require.Equal(t, "c", elem.Value.(*Entry).Key)
	require.Equal(t, int32(1), elem.Value.(*Entry).refCount.Load())

}

func TestBlocksCache_TTLEviction(t *testing.T) {
	cfg := config.BlocksCacheConfig{
		TTL:       100 * time.Millisecond,
		SoftLimit: flagext.Bytes(10),
		HardLimit: flagext.Bytes(20),

		PurgeInterval: 100 * time.Millisecond,
	}
	cache := NewFsBlocksCache(cfg, nil, logger)
	t.Cleanup(cache.Stop)

	ctx := context.Background()

	err := cache.Put(ctx, "a", CacheValue("a", 5))
	require.NoError(t, err)
	time.Sleep(75 * time.Millisecond)

	err = cache.Put(ctx, "b", CacheValue("b", 5))
	require.NoError(t, err)
	time.Sleep(75 * time.Millisecond)

	// "a" got evicted
	_, found := cache.Get(ctx, "a")
	require.False(t, found)

	// "b" is still in cache
	_, found = cache.Get(ctx, "b")
	require.True(t, found)

	require.Equal(t, 1, cache.lru.Len())
	require.Equal(t, 1, len(cache.entries))
}

func TestBlocksCache_LRUEviction(t *testing.T) {
	cfg := config.BlocksCacheConfig{
		TTL:       time.Hour,
		SoftLimit: flagext.Bytes(10),
		HardLimit: flagext.Bytes(20),
		// no need for TTL evictions
		PurgeInterval: time.Minute,
	}
	cache := NewFsBlocksCache(cfg, nil, logger)
	t.Cleanup(cache.Stop)

	ctx := context.Background()

	err := cache.PutMany(
		ctx,
		[]string{"a", "b"},
		[]BlockDirectory{CacheValue("a", 4), CacheValue("b", 4)},
	)
	require.NoError(t, err)

	// increase ref counter on "a"
	_, found := cache.Get(ctx, "a")
	require.True(t, found)

	// exceed soft limit
	err = cache.Put(ctx, "c", CacheValue("c", 4))
	require.NoError(t, err)

	time.Sleep(time.Second)

	require.Equal(t, 2, cache.lru.Len())
	require.Equal(t, 2, len(cache.entries))

	// key "b" was evicted because it was the oldest
	// and it had no ref counts
	_, found = cache.Get(ctx, "b")
	require.False(t, found)

	require.Equal(t, int64(8), cache.currSizeBytes)
}

func TestBlocksCache_RefCounter(t *testing.T) {
	cfg := config.BlocksCacheConfig{
		TTL:       time.Hour,
		SoftLimit: flagext.Bytes(10),
		HardLimit: flagext.Bytes(20),
		// no need for TTL evictions
		PurgeInterval: time.Minute,
	}
	cache := NewFsBlocksCache(cfg, nil, logger)
	t.Cleanup(cache.Stop)

	ctx := context.Background()

	_ = cache.Put(ctx, "a", CacheValue("a", 5))
	require.Equal(t, int32(0), cache.entries["a"].Value.(*Entry).refCount.Load())

	_, _ = cache.Get(ctx, "a")
	require.Equal(t, int32(1), cache.entries["a"].Value.(*Entry).refCount.Load())

	_, _ = cache.Get(ctx, "a")
	require.Equal(t, int32(2), cache.entries["a"].Value.(*Entry).refCount.Load())

	_ = cache.Release(ctx, "a")
	require.Equal(t, int32(1), cache.entries["a"].Value.(*Entry).refCount.Load())

	_ = cache.Release(ctx, "a")
	require.Equal(t, int32(0), cache.entries["a"].Value.(*Entry).refCount.Load())
}
