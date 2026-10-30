package capability

// Legacy ticket compatibility contract: the existing identity.TicketService
// remains authoritative and unchanged until a single transaction can guard
// fallback against a concurrent new-header insertion and legacy consumption.
// A separate absence check followed by a legacy consume is not sufficient.
// No production adapter or table cutover is enabled by this package.
