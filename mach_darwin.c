//go:build darwin && cgo

#include "mach_darwin.h"

#include <mach/mach_vm.h>
#include <servers/bootstrap.h>
#include <string.h>

// From launchd's bootstrap_priv.h.
#define BOOTSTRAP_PRIVILEGED_SERVER 0x8
kern_return_t bootstrap_look_up2(mach_port_t bp, const name_t name, mach_port_t *sp, pid_t pid, uint64_t flags);

// From sys/fileport.h.
int fileport_makeport(int fd, mach_port_t *port);
int fileport_makefd(mach_port_t port);

// The id of the check-in message, "w00t".
#define CHECKIN_ID 0x77303074

// Messages are received with the audit trailer, whose token the kernel
// fills in with the sender's identity: it is how notifications are told
// from forgeries, and how peers are checked. A message too large to
// receive stays queued, and the header tells its size and, received on a
// port set, the member port it is queued on.
#define RCV_OPTIONS                                                                                    \
	(MACH_RCV_MSG | MACH_RCV_LARGE | MACH_RCV_LARGE_IDENTITY |                                      \
	 MACH_RCV_TRAILER_TYPE(MACH_MSG_TRAILER_FORMAT_0) | MACH_RCV_TRAILER_ELEMENTS(MACH_RCV_TRAILER_AUDIT))

kern_return_t goxpc_lookup(const char *name, int privileged, mach_port_t *port) {
	if (privileged) {
		return bootstrap_look_up2(bootstrap_port, name, port, 0, BOOTSTRAP_PRIVILEGED_SERVER);
	}
	return bootstrap_look_up(bootstrap_port, name, port);
}

kern_return_t goxpc_checkin(mach_port_t service, mach_port_t *srv, mach_port_t *cli) {
	mach_port_t task = mach_task_self();
	mach_port_t s, c, prev;
	kern_return_t kr = mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &s);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	kr = mach_port_insert_right(task, s, s, MACH_MSG_TYPE_MAKE_SEND);
	if (kr != KERN_SUCCESS) {
		goto fail_s;
	}
	if (*cli == MACH_PORT_NULL) {
		kr = mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, &c);
		if (kr != KERN_SUCCESS) {
			goto fail_s;
		}
	} else {
		c = *cli; // a reconnection keeps the client's port
	}

	struct {
		mach_msg_header_t h;
		mach_msg_body_t b;
		mach_msg_port_descriptor_t p[2];
	} m = {0};
	m.h.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0) | MACH_MSGH_BITS_COMPLEX;
	m.h.msgh_size = sizeof m;
	m.h.msgh_remote_port = service;
	m.h.msgh_id = CHECKIN_ID;
	m.b.msgh_descriptor_count = 2;
	m.p[0].name = s;
	m.p[0].disposition = MACH_MSG_TYPE_MOVE_RECEIVE;
	m.p[0].type = MACH_MSG_PORT_DESCRIPTOR;
	m.p[1].name = c;
	m.p[1].disposition = MACH_MSG_TYPE_MAKE_SEND;
	m.p[1].type = MACH_MSG_PORT_DESCRIPTOR;
	kr = mach_msg(&m.h, MACH_SEND_MSG, sizeof m, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
	if (kr != KERN_SUCCESS) {
		if (*cli == MACH_PORT_NULL) mach_port_mod_refs(task, c, MACH_PORT_RIGHT_RECEIVE, -1);
		goto fail_s;
	}
	// s now names only our send right; it becomes a dead name, and cli
	// is told, when the server's side goes away.
	kr = mach_port_request_notification(task, s, MACH_NOTIFY_DEAD_NAME, 1, c, MACH_MSG_TYPE_MAKE_SEND_ONCE, &prev);
	if (kr == KERN_INVALID_ARGUMENT) {
		// Already dead: the notification is not armed, report it at once.
		kr = KERN_SUCCESS;
	}
	*srv = s;
	*cli = c;
	return kr;

fail_s:
	mach_port_mod_refs(task, s, MACH_PORT_RIGHT_RECEIVE, -1);
	mach_port_deallocate(task, s);
	return kr;
}

kern_return_t goxpc_send(void *buf, mach_msg_size_t size) {
	return mach_msg((mach_msg_header_t *)buf, MACH_SEND_MSG, size, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE,
	                MACH_PORT_NULL);
}

kern_return_t goxpc_send_receive(void *buf, mach_msg_size_t size, mach_msg_size_t cap, mach_port_t port) {
	return mach_msg((mach_msg_header_t *)buf, MACH_SEND_MSG | RCV_OPTIONS, size, cap, port, MACH_MSG_TIMEOUT_NONE,
	                MACH_PORT_NULL);
}

kern_return_t goxpc_receive(void *buf, mach_msg_size_t cap, mach_port_t port) {
	return mach_msg((mach_msg_header_t *)buf, RCV_OPTIONS, 0, cap, port, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

kern_return_t goxpc_discard(mach_port_t port) {
	// Received without MACH_RCV_LARGE into a buffer too small for it, the
	// message is dequeued and destroyed by the kernel.
	mach_msg_header_t h;
	return mach_msg(&h, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof h, port, 0, MACH_PORT_NULL);
}

void goxpc_destroy(void *buf) { mach_msg_destroy((mach_msg_header_t *)buf); }

kern_return_t goxpc_send_raw(mach_port_t port, mach_msg_id_t id, const void *body, mach_msg_size_t n) {
	struct {
		mach_msg_header_t h;
		uint8_t body[256];
	} m = {0};
	if (n > sizeof m.body) {
		return KERN_INVALID_ARGUMENT;
	}
	m.h.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MAKE_SEND, 0);
	m.h.msgh_size = sizeof m.h + ((n + 3) & ~3u);
	m.h.msgh_remote_port = port;
	m.h.msgh_id = id;
	memcpy(m.body, body, n);
	return mach_msg(&m.h, MACH_SEND_MSG, m.h.msgh_size, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

kern_return_t goxpc_wake(mach_port_t port, mach_msg_id_t id) {
	mach_msg_header_t h = {0};
	// Sent on a send-once right, the message is queued even when the port's
	// queue is full: the sender, which may hold a lock the receiver needs,
	// never waits for the receiver.
	h.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MAKE_SEND_ONCE, 0);
	h.msgh_size = sizeof h;
	h.msgh_remote_port = port;
	h.msgh_id = id;
	return mach_msg(&h, MACH_SEND_MSG, sizeof h, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

kern_return_t goxpc_listen(mach_port_t *port, mach_port_t *set) {
	mach_port_t task = mach_task_self();
	kern_return_t kr = mach_port_allocate(task, MACH_PORT_RIGHT_PORT_SET, set);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	kr = mach_port_allocate(task, MACH_PORT_RIGHT_RECEIVE, port);
	if (kr != KERN_SUCCESS) {
		goto fail_set;
	}
	kr = mach_port_insert_member(task, *port, *set);
	if (kr != KERN_SUCCESS) {
		mach_port_mod_refs(task, *port, MACH_PORT_RIGHT_RECEIVE, -1);
		goto fail_set;
	}
	return KERN_SUCCESS;

fail_set:
	mach_port_mod_refs(task, *set, MACH_PORT_RIGHT_PORT_SET, -1);
	return kr;
}

kern_return_t goxpc_accept(mach_port_t port, mach_port_t set) {
	mach_port_t task = mach_task_self(), prev = MACH_PORT_NULL;
	kern_return_t kr = mach_port_insert_member(task, port, set);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	kr = mach_port_request_notification(task, port, MACH_NOTIFY_NO_SENDERS, 0, port, MACH_MSG_TYPE_MAKE_SEND_ONCE,
	                                    &prev);
	if (prev != MACH_PORT_NULL) {
		// A request the peer armed before it moved the right.
		mach_port_deallocate(task, prev);
	}
	return kr;
}

kern_return_t goxpc_make_send(mach_port_t port) {
	return mach_port_insert_right(mach_task_self(), port, port, MACH_MSG_TYPE_MAKE_SEND);
}

int goxpc_port_dead(mach_port_t port) {
	mach_port_type_t type;
	return mach_port_type(mach_task_self(), port, &type) == KERN_SUCCESS && (type & MACH_PORT_TYPE_DEAD_NAME) != 0;
}

void goxpc_set_destroy(mach_port_t set) {
	mach_port_mod_refs(mach_task_self(), set, MACH_PORT_RIGHT_PORT_SET, -1);
}

mach_port_t goxpc_port_alloc(void) {
	mach_port_t p = MACH_PORT_NULL;
	mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &p);
	return p;
}

void goxpc_port_destroy(mach_port_t port) {
	mach_port_mod_refs(mach_task_self(), port, MACH_PORT_RIGHT_RECEIVE, -1);
}

void goxpc_port_release(mach_port_t port) {
	mach_port_deallocate(mach_task_self(), port);
}

kern_return_t goxpc_copy_pages(const void *data, size_t n, uintptr_t *addr) {
	mach_vm_address_t a = 0;
	kern_return_t kr = mach_vm_allocate(mach_task_self(), &a, n, VM_FLAGS_ANYWHERE);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	memcpy((void *)a, data, n);
	*addr = (uintptr_t)a;
	return KERN_SUCCESS;
}

kern_return_t goxpc_memory_entry(const void *data, size_t n, mach_port_t *port) {
	uintptr_t a;
	kern_return_t kr = goxpc_copy_pages(data, n, &a);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	memory_object_size_t size = n;
	kr = mach_make_memory_entry_64(mach_task_self(), &size, a, VM_PROT_READ | MAP_MEM_VM_COPY, port, MACH_PORT_NULL);
	mach_vm_deallocate(mach_task_self(), a, n);
	return kr;
}

kern_return_t goxpc_map(mach_port_t port, size_t n, uintptr_t *addr) {
	mach_vm_address_t a = 0;
	kern_return_t kr = mach_vm_map(mach_task_self(), &a, n, 0, VM_FLAGS_ANYWHERE, port, 0, FALSE, VM_PROT_READ,
	                               VM_PROT_READ, VM_INHERIT_NONE);
	*addr = (uintptr_t)a;
	return kr;
}

void goxpc_unmap(uintptr_t addr, size_t n) {
	mach_vm_deallocate(mach_task_self(), addr, n);
}

int goxpc_fileport_make(int fd, mach_port_t *port) {
	return fileport_makeport(fd, port);
}

int goxpc_fileport_fd(mach_port_t port) {
	return fileport_makefd(port);
}
