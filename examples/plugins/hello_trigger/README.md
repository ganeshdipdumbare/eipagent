# hello_trigger

hello_trigger listens for webhooks and starts an agent run per request. Senders sign "<X-Timestamp>.<body>" with HMAC-SHA256 and send X-Signature: sha256=<hex>. Requests older than 5 minutes, replays of a request already accepted, and more than max_concurrent runs at once are refused.

A plugin made with `eip plugin init` to start runs from outside (a signed webhook).

```bash
eip plugin validate .
eip plugin build -key ~/.eip/signing.key .
```
