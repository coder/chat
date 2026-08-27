package chat

import "sync"

// tenantKey is the ADR 0006 installation identity the per-tenant admission
// ceiling is keyed on. An empty tenant is a valid key: a single-tenant
// adapter's deliveries are capped under (adapter, "") rather than escaping
// the ceiling or colliding with another adapter's empty tenant.
type tenantKey struct {
	adapter string
	tenant  string
}

// admissionGate is the deferred-dispatch admission bound (ADR 0015): a
// per-instance cap on admitted-but-incomplete deferred deliveries, with an
// optional per-installation ceiling through the same rejection path. It is
// constructed only under DispatchDeferred and closed by Runtime Shutdown so
// a shutting-down instance sheds new deliveries to the platform's retry
// instead of admitting work it is about to cancel.
type admissionGate struct {
	mu sync.Mutex
	// capacity is MaxDetached; perTenant is MaxDetachedPerTenant (0 = disabled).
	capacity  int
	perTenant int
	inUse     int
	// tenants counts held slots per installation; entries are deleted when a
	// tenant's count reaches zero so an idle tenant retains no map entry.
	tenants map[tenantKey]int
	closed  bool
	// drainDone is created by close and closed once every held slot has been
	// released, so Runtime Shutdown can wait for admitted deliveries that are
	// still in their prelude (not yet counted by the tail WaitGroup) as well
	// as running tails.
	drainDone chan struct{}
}

func newAdmissionGate(capacity, perTenant int) *admissionGate {
	assert(capacity > 0, "admission gate capacity must be positive")
	assert(perTenant >= 0, "admission per-tenant ceiling must not be negative")
	gate := &admissionGate{capacity: capacity, perTenant: perTenant}
	if perTenant > 0 {
		gate.tenants = map[tenantKey]int{}
	}
	return gate
}

// admit reserves one admission slot for a deferred delivery. It reports false
// when the gate is closed (Runtime Shutdown), the instance is at capacity, or
// the delivery's installation is at its per-tenant ceiling. The returned
// release is idempotent and must be called exactly when the delivery stops
// retaining work: when its prelude resolves or fails, or when its detached
// tail goroutine returns.
func (g *admissionGate) admit(adapter, tenant string) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.inUse >= g.capacity {
		return nil, false
	}
	key := tenantKey{adapter: adapter, tenant: tenant}
	if g.perTenant > 0 && g.tenants[key] >= g.perTenant {
		return nil, false
	}
	g.inUse++
	if g.perTenant > 0 {
		g.tenants[key]++
	}
	return sync.OnceFunc(func() { g.release(key) }), true
}

func (g *admissionGate) release(key tenantKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	assert(g.inUse > 0, "admission release without a held slot")
	g.inUse--
	if g.closed && g.inUse == 0 && g.drainDone != nil {
		close(g.drainDone)
	}
	if g.perTenant > 0 {
		count := g.tenants[key]
		assert(count > 0, "admission release without a held tenant slot")
		if count == 1 {
			delete(g.tenants, key)
		} else {
			g.tenants[key] = count - 1
		}
	}
}

// close permanently rejects new admissions. Held slots are unaffected: their
// releases still run so accounting stays exact while in-flight work drains.
// The returned channel is closed once every held slot has been released —
// including slots held by deliveries still in their synchronous prelude, which
// the tail WaitGroup cannot see — so Runtime Shutdown does not tear down
// adapters and state under an admitted delivery that won the shutdown race.
func (g *admissionGate) close() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.drainDone == nil {
		g.drainDone = make(chan struct{})
		if g.inUse == 0 {
			close(g.drainDone)
		}
	}
	return g.drainDone
}

// tenantEntries reports the number of live per-tenant counter entries, for
// tests proving released tenants are garbage collected.
func (g *admissionGate) tenantEntries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tenants)
}
