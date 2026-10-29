package chat

// AdmissionTenantEntries reports the number of live per-tenant admission
// counter entries, for hardening tests proving a tenant whose deliveries all
// released capacity retains no counter entry (ADR 0015 bounded retention).
func AdmissionTenantEntries(c *Chat) int {
	assert(c != nil, "AdmissionTenantEntries called on nil runtime")
	return c.admission.tenantEntries()
}

// BurstScopeCount reports the number of live burst scope coordinators, for
// hardening tests proving an idle scope retains no map entry (ADR 0015 bounded
// retention).
func BurstScopeCount(c *Chat) int {
	assert(c != nil, "BurstScopeCount called on nil runtime")
	c.burstMu.Lock()
	defer c.burstMu.Unlock()
	return len(c.burstScopes)
}

// QueueTurnCount reports the number of live queue turn records, for hardening
// tests proving an idle scope retains no record (ADR 0015 bounded retention).
func QueueTurnCount(c *Chat) int {
	assert(c != nil, "QueueTurnCount called on nil runtime")
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	return len(c.queueTurns)
}

// QueueDeferredRelease counts one queue delivery in scope's turn record, as
// the prelude does, and returns the release dispatch gives that delivery's
// deferred path.
func QueueDeferredRelease(c *Chat, scope string) func() {
	assert(c != nil, "QueueDeferredRelease called on nil runtime")
	entry := &queueEntry{}
	c.enterQueueScope(scope, entry)
	return c.deferredRelease(entry, func() {})
}
