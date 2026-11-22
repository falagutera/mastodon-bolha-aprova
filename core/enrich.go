package core

import (
	"context"
	"sync"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
)

// checkIPShared fails when the app acted against another account at the
// sign-up's IP. It is shown on the request page only, and can't be required.
const checkIPShared = "ip_shared"

// enrich runs the lookups for the page of one pending request, in parallel and
// within lookupTimeout. Whatever doesn't answer in time is shown as unavailable.
// Only buildRequest calls it, and only while the request is pending (rule 1).
func (s *Server) enrich(ctx context.Context, sess session, svc *service, q module.Request) (*originView, []breachView) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var origin *originView
	var breaches []breachView
	var wg sync.WaitGroup
	if q.IP != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			origin = s.lookups.origin(ctx, q.IP)
			if origin == nil {
				return
			}
			finder, ok := svc.mod.(module.SameIP)
			if !ok {
				return
			}
			origin.Instance = svc.cfg.Name
			m, err := finder.SameIP(ctx, actorFor(sess, svc), q.ID, origin.IP)
			if err != nil {
				origin.SameIPDown = true
				return
			}
			origin.SameIP, origin.SameIPMore, origin.SameIPURL, origin.SameIPSeen = m.Counts, m.More, m.AdminURL, true
		}()
	}
	if (len(s.lookups.cfg.Breaches) > 0 || s.lookups.cfg.hibpWanted) && q.Email != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			breaches = s.lookups.breaches(ctx, q.Email)
		}()
	}
	wg.Wait()
	return origin, breaches
}
