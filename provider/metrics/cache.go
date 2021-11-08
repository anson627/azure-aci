package metrics

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	stats "github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	v1 "k8s.io/api/core/v1"
)

func WrapCachedPodStatsGetter(ttlSeconds int, getter podStatsGetter) *cachePodStatsGetter {
	return &cachePodStatsGetter{
		wrappedGetter: getter,
		cache:         cache.New(time.Duration(ttlSeconds)*time.Second, 10*time.Minute),
	}
}

//Adding cache capability into podStatsGetter
type cachePodStatsGetter struct {
	wrappedGetter podStatsGetter
	cache         *cache.Cache
}

func (cacheGetter *cachePodStatsGetter) getPodStats(ctx context.Context, pod *v1.Pod) (*stats.PodStats, error) {
	cacheKey := string(pod.UID)
	cachedPodStats, found := cacheGetter.cache.Get(cacheKey)
	if found {
		return cachedPodStats.(*stats.PodStats), nil
	}
	stats, err := cacheGetter.wrappedGetter.getPodStats(ctx, pod)
	if err != nil {
		return nil, err
	}
	cacheGetter.cache.Set(cacheKey, stats, cache.DefaultExpiration)
	return stats, nil
}
