#ifndef HSA_WRAPPER_H
#define HSA_WRAPPER_H

#include "hsa_ext_amd.h"

#ifdef __cplusplus
extern "C" {
#endif

extern hsa_status_t go_hsa_iterate_agents_cb(hsa_agent_t agent, void* data);

static inline hsa_status_t call_hsa_iterate_agents(void* data) {
    return hsa_iterate_agents(go_hsa_iterate_agents_cb, data);
}

#ifdef __cplusplus
}
#endif

#endif
