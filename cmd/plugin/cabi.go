//go:build cshared

package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int guardCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void guardFree(void*, size_t);
extern void guardShutdown(void);
*/
import "C"
import (
	"cpa-codex-candy-guard/internal/guard"
	"unsafe"
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != 1 {
		return 1
	}
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.guardCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.guardFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.guardShutdown)
	return 0
}

//export guardCall
func guardCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) C.int {
	if method == nil || response == nil || length > 16<<20 {
		return 1
	}
	var raw []byte
	if request != nil && length > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	result := guard.HandleMethod(C.GoString(method), raw)
	response.ptr = C.CBytes(result)
	response.len = C.size_t(len(result))
	return 0
}

//export guardFree
func guardFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export guardShutdown
func guardShutdown() { guard.Quiesce() }
