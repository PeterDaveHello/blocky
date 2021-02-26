package resolver

import (
	"blocky/config"
	"blocky/evt"
	"blocky/util"
	"fmt"
	"time"

	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

// CachingResolver caches answers from dns queries with their TTL time,
// to avoid external resolver calls for recurrent queries
type CachingResolver struct {
	NextResolver
	minCacheTimeSec, maxCacheTimeSec int
	cachesPerType                    map[uint16]*cache.Cache
	prefetchingNameCache             *cache.Cache
}

const (
	cacheTimeNegative              = 30 * time.Minute
	prefetchingNameCacheExpiration = 2 * time.Hour
	prefetchingNameCountThreshold  = 5
)

// NewCachingResolver creates a new resolver instance
func NewCachingResolver(cfg config.CachingConfig) ChainedResolver {
	domainCache := createQueryDomainNameCache(cfg)
	c := &CachingResolver{
		minCacheTimeSec: 60 * cfg.MinCachingTime,
		maxCacheTimeSec: 60 * cfg.MaxCachingTime,
		cachesPerType: map[uint16]*cache.Cache{
			dns.TypeA:    createQueryResultCache(),
			dns.TypeAAAA: createQueryResultCache(),
		},
		prefetchingNameCache: domainCache,
	}

	if cfg.Prefetching {
		configurePrefetching(c)
	}

	return c
}

func configurePrefetching(c *CachingResolver) {
	for k, v := range c.cachesPerType {
		qType := k

		v.OnEvicted(func(domainName string, i interface{}) {
			c.onEvicted(domainName, qType)
		})
	}
}

func createQueryResultCache() *cache.Cache {
	return cache.New(15*time.Minute, 15*time.Second)
}
func createQueryDomainNameCache(cfg config.CachingConfig) *cache.Cache {
	if cfg.Prefetching {
		return cache.New(prefetchingNameCacheExpiration, time.Minute)
	}

	return nil
}

// onEvicted is called if a DNS response in the cache is expired and was removed from cache
func (r *CachingResolver) onEvicted(domainName string, qType uint16) {
	logger := logger("caching_resolver")

	cnt, found := r.prefetchingNameCache.Get(domainName)

	// check if domain was queried > threshold in the time window
	if found && cnt.(int) > prefetchingNameCountThreshold {
		logger.Debugf("prefetching '%s' (%s)", domainName, dns.TypeToString[qType])

		req := newRequest(fmt.Sprintf("%s.", domainName), qType, logger)
		response, err := r.next.Resolve(req)

		if err == nil {
			r.putInCache(response, domainName, qType)

			evt.Bus().Publish(evt.CachingDomainPrefetched, domainName)
		}

		util.LogOnError(fmt.Sprintf("can't prefetch '%s' ", domainName), err)
	}
}

func (r *CachingResolver) getCache(queryType uint16) *cache.Cache {
	return r.cachesPerType[queryType]
}

// Configuration returns a current resolver configuration
func (r *CachingResolver) Configuration() (result []string) {
	if r.maxCacheTimeSec < 0 {
		result = []string{"deactivated"}
		return
	}

	result = append(result, fmt.Sprintf("minCacheTimeInSec = %d", r.minCacheTimeSec))

	result = append(result, fmt.Sprintf("maxCacheTimeSec = %d", r.maxCacheTimeSec))

	result = append(result, fmt.Sprintf("prefetching = %t", r.prefetchingNameCache != nil))

	for t, c := range r.cachesPerType {
		result = append(result, fmt.Sprintf("%s cache items count = %d", dns.TypeToString[t], c.ItemCount()))
	}

	return
}

func (r *CachingResolver) getTotalCacheEntryNumber() int {
	count := 0
	for _, v := range r.cachesPerType {
		count += v.ItemCount()
	}

	return count
}

// Resolve checks if the current query result is already in the cache and returns it
// or delegates to the next resolver
//nolint:gocognit,funlen
func (r *CachingResolver) Resolve(request *Request) (response *Response, err error) {
	logger := withPrefix(request.Log, "caching_resolver")

	if r.maxCacheTimeSec < 0 {
		logger.Debug("skip cache")
		return r.next.Resolve(request)
	}

	resp := new(dns.Msg)
	resp.SetReply(request.Req)

	for _, question := range request.Req.Question {
		domain := util.ExtractDomain(question)
		logger := logger.WithField("domain", domain)

		// we can cache only A and AAAA queries
		if question.Qtype == dns.TypeA || question.Qtype == dns.TypeAAAA {
			r.trackQueryDomainNameCount(domain, logger)

			val, expiresAt, found := r.getCache(question.Qtype).GetWithExpiration(domain)

			if found {
				logger.Debug("domain is cached")

				evt.Bus().Publish(evt.CachingResultCacheHit, domain)

				// calculate remaining TTL
				remainingTTL := uint32(time.Until(expiresAt).Seconds())

				v, ok := val.([]dns.RR)
				if ok {
					// Answer from successful request
					resp.Answer = v
					for _, rr := range resp.Answer {
						rr.Header().Ttl = remainingTTL
					}

					return &Response{Res: resp, RType: CACHED, Reason: "CACHED"}, nil
				}
				// Answer with response code != OK
				resp.Rcode = val.(int)

				return &Response{Res: resp, RType: CACHED, Reason: "CACHED NEGATIVE"}, nil
			}

			evt.Bus().Publish(evt.CachingResultCacheMiss, domain)

			logger.WithField("next_resolver", Name(r.next)).Debug("not in cache: go to next resolver")
			response, err = r.next.Resolve(request)

			if err == nil {
				r.putInCache(response, domain, question.Qtype)
			}
		} else {
			logger.Debugf("not A/AAAA: go to next %s", r.next)
			return r.next.Resolve(request)
		}
	}

	return response, err
}

func (r *CachingResolver) trackQueryDomainNameCount(domain string, logger *logrus.Entry) {
	if r.prefetchingNameCache != nil {
		var domainCount int
		if x, found := r.prefetchingNameCache.Get(domain); found {
			domainCount = x.(int)
		}
		domainCount++
		r.prefetchingNameCache.SetDefault(domain, domainCount)
		logger.Debugf("domain '%s' was requested %d times, "+
			"total cache size: %d", domain, domainCount, r.prefetchingNameCache.ItemCount())
		evt.Bus().Publish(evt.CachingDomainsToPrefetchCountChanged, r.prefetchingNameCache.ItemCount())
	}
}

func (r *CachingResolver) putInCache(response *Response, domain string, qType uint16) {
	answer := response.Res.Answer

	if response.Res.Rcode == dns.RcodeSuccess {
		// put value into cache
		r.getCache(qType).Set(domain, answer, time.Duration(r.adjustTTLs(answer))*time.Second)
	} else if response.Res.Rcode == dns.RcodeNameError {
		// put return code if NXDOMAIN
		r.getCache(qType).Set(domain, response.Res.Rcode, cacheTimeNegative)
	}

	evt.Bus().Publish(evt.CachingResultCacheChanged, r.getTotalCacheEntryNumber())
}

func (r *CachingResolver) adjustTTLs(answer []dns.RR) (maxTTL uint32) {
	for _, a := range answer {
		// if TTL < mitTTL -> adjust the value, set minTTL
		if r.minCacheTimeSec > 0 {
			if a.Header().Ttl < uint32(r.minCacheTimeSec) {
				a.Header().Ttl = uint32(r.minCacheTimeSec)
			}
		}

		if r.maxCacheTimeSec > 0 {
			if a.Header().Ttl > uint32(r.maxCacheTimeSec) {
				a.Header().Ttl = uint32(r.maxCacheTimeSec)
			}
		}

		if maxTTL < a.Header().Ttl {
			maxTTL = a.Header().Ttl
		}
	}

	return
}
