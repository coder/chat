package chat

// AdmissionTenantEntries reports the number of live per-tenant admission
// counter entries, for hardening tests proving a tenant whose deliveries all
// released capacity retains no counter entry (ADR 0015 bounded retention).
func AdmissionTenantEntries(c *Chat) int {
	assert(c != nil, "AdmissionTenantEntries called on nil runtime")
	if c.admission == nil {
		return 0
	}
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
