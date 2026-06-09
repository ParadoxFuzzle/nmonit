# C SDK Design

## Principles
- Header-only for simple cases, static library for performance
- Every call non-blocking by default (completion queue model)
- Zero-copy where possible (RDMA, mmap)

## API Surface (Phase 1)

```c
// Memory
void* distributed_malloc(size_t size);
void  distributed_free(void* ptr);
int   distributed_get_info(void* ptr, mem_info_t* info);

// Communication (TCP-based for Phase 1)
int   fabric_send(node_id_t node, const void* buf, size_t len);
int   fabric_recv(node_id_t node, void* buf, size_t len);

// Job submission
int   fabric_submit_job(job_spec_t* spec, job_id_t* out_id);
int   fabric_get_job_status(job_id_t id, job_status_t* status);
```

See `sdk/c/include/fabric.h` for full API.
