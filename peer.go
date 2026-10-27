package xpc

// Peer identifies the process that sent a message, as the kernel records
// it: a sender cannot forge it.
type Peer struct {
	PID        int
	UID, GID   uint32    // effective user and group IDs
	AuditToken [8]uint32 // the sender's audit_token_t
}
