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
