# hello_model

hello_model is a model provider for an OpenAI-compatible chat API, registered as "model/hello_model": use models as "hello_model:<model>". Text only; see llm_openai for tools and streaming.

A plugin made with `eip plugin init` to connect a model provider (an OpenAI-compatible API).

```bash
eip plugin validate .
eip plugin build -key ~/.eip/signing.key .
```
