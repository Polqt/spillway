# 04: Model listing and model-reference errors

**What to build:** A client calls `GET /v1/models` and sees every model of every configured provider as a model reference (`provider/model`), in the OpenAI list shape.
A request naming a virtual model (`auto`, `spillway/<tier>`) gets an OpenAI-shaped 404 explaining that virtual models arrive in v0.3 and pointing at `/v1/models`.
A model reference with an unknown provider, or a name with no provider prefix, gets a 404 that says what to send instead.
Design: plan "internal/api" (model resolution, `/v1/models`), decisions 6 and 7.

**Blocked by:** 02: Mock chat end to end.

**Status:** ready-for-agent

- [x] `GET /v1/models` requires a local key and returns `{"object":"list","data":[...]}` with ids such as `mock/echo`, in a stable order.
- [x] If one provider fails to list its models, the others are still returned and the failure is logged (scrubbed) at warn; only when every provider fails is the response 502.
- [x] Virtual models are not listed in v0.1.
- [x] `auto`, `spillway/cheap`, `unknown/x` and `llama3` each return 404 `model_not_found` with a message naming the fix.
- [x] Model references whose upstream model contains `/` or `:` split at the first `/` only.
- [x] `make check` passes.
