#ifndef GOXPC_MACH_H
#define GOXPC_MACH_H

// Thin wrappers around the Mach and launchd calls the XPC protocol needs.
// Messages themselves are laid out by Go (see mach_darwin.go); C only
// passes them to the kernel.

#include <mach/mach.h>
#include <stddef.h>
#include <stdint.h>

// goxpc_lookup finds the port of the launchd service name. A privileged
// lookup only finds daemons of the system domain.
kern_return_t goxpc_lookup(const char *name, int privileged, mach_port_t *port);

// goxpc_checkin opens a connection to the service listening on service:
// it allocates the server's receive right (*srv, of which the caller keeps
// a send right) and the client's (*cli, of which the server gets a send
// right), sends both in a check-in message, and asks for a dead-name
// notification on cli when srv dies.
kern_return_t goxpc_checkin(mach_port_t service, mach_port_t *srv, mach_port_t *cli);

// goxpc_send sends the message in buf.
kern_return_t goxpc_send(void *buf, mach_msg_size_t size);

// goxpc_send_receive sends the message in buf, then receives on port into
// buf, which holds cap bytes, with an audit trailer after the message. If
// the message received is larger, it stays queued and MACH_RCV_TOO_LARGE
// is returned with its size, and the port it is queued on, in the header.
kern_return_t goxpc_send_receive(void *buf, mach_msg_size_t size, mach_msg_size_t cap, mach_port_t port);

// goxpc_receive receives on port into buf, like goxpc_send_receive.
kern_return_t goxpc_receive(void *buf, mach_msg_size_t cap, mach_port_t port);

// goxpc_discard dequeues and destroys the next message on port, one too
// large to receive.
kern_return_t goxpc_discard(mach_port_t port);

// goxpc_destroy releases the port rights and out-of-line memory of the
// message received in buf, whatever its descriptors.
void goxpc_destroy(void *buf);

// goxpc_send_raw sends a message of id with body inline to port, of which
// the caller holds the receive right. It is for the tests.
kern_return_t goxpc_send_raw(mach_port_t port, mach_msg_id_t id, const void *body, mach_msg_size_t n);

// goxpc_wake sends an empty message with the given id to port, of which
// the caller holds the receive right. It does not wait for room in the
// port's queue.
kern_return_t goxpc_wake(mach_port_t port, mach_msg_id_t id);

// goxpc_listen allocates the receive right a listener's peers check in on,
// *port, and a port set, *set, that it is a member of.
kern_return_t goxpc_listen(mach_port_t *port, mach_port_t *set);

// goxpc_accept makes port, the receive right of a peer's messages, a
// member of set, and asks for a no-senders notification, sent to port
// itself, for when the peer has gone.
kern_return_t goxpc_accept(mach_port_t port, mach_port_t set);

// goxpc_make_send makes a send right to port, of which the caller holds
// the receive right.
kern_return_t goxpc_make_send(mach_port_t port);

void goxpc_set_destroy(mach_port_t set); // drops a port set

// goxpc_port_dead reports whether port is a dead name: the receive right of
// the send right it named was destroyed.
int goxpc_port_dead(mach_port_t port);

mach_port_t goxpc_port_alloc(void);
void goxpc_port_destroy(mach_port_t port); // drops a receive right
void goxpc_port_release(mach_port_t port); // drops a send right

// goxpc_memory_entry copies n bytes into fresh pages and returns a memory
// entry for them in *port, the way XPC passes large data.
kern_return_t goxpc_memory_entry(const void *data, size_t n, mach_port_t *port);

// goxpc_map maps n bytes of the memory entry port read-only.
kern_return_t goxpc_map(mach_port_t port, size_t n, uintptr_t *addr);

// goxpc_copy_pages copies n bytes into fresh pages, to send as an
// out-of-line message body.
kern_return_t goxpc_copy_pages(const void *data, size_t n, uintptr_t *addr);

void goxpc_unmap(uintptr_t addr, size_t n);

// File descriptors travel as fileports.
int goxpc_fileport_make(int fd, mach_port_t *port);
int goxpc_fileport_fd(mach_port_t port);

#endif
