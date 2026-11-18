# T3 protocol gateway

`t3fleet` exposes one stable HTTP origin per machine. T3 Code pairs with that
origin rather than the provider's hostname. The first implementation relays a
live T3 server; it does not impersonate a sleeping server or replay old chats.

The gateway follows the T3 orchestration protocol version 2 used by the pinned
T3 server. The HTTP routes and Effect RPC frame conventions were studied in
[t3boat](https://github.com/paukraft/t3boat), which is MIT licensed by Pau Kraft.
The gateway is an independent Go implementation. Compatibility with a new T3
server version must be tested before changing the image pin.

## Connection and authorization

1. The operator requests a pairing link from the fleet control interface.
   `Proxy.NewPairing` creates a 256-bit code valid for 15 minutes. A new link
   invalidates the previous unredeemed link.
2. T3 reads `GET /.well-known/t3/environment`. The gateway resolves the live
   machine, fetches its descriptor, and substitutes the fleet machine label.
3. T3 submits the pairing code as `subject_token` to `POST /oauth/token`.
   The gateway consumes the code exactly once and persists a new downstream
   bearer token. The code and the returned token must not enter logs.
4. `GET /api/auth/session` validates that downstream token and obtains the
   remote session using the gateway's upstream service credential.
5. `POST /api/auth/websocket-ticket/rpc` creates a one-use socket ticket.
   The `media` variant creates a ticket usable on device/preview streams for
   five minutes. A media ticket does not authorize ordinary HTTP API routes.
6. T3 opens `/ws?wsTicket=...`. The gateway consumes the RPC ticket, resolves
   the machine, and opens an authenticated WebSocket to its T3 server.
   The local ticket is removed before forwarding. The remote service token
   replaces the client's bearer token.

The downstream bearer token is distinct from the service credential on the VM.
Persistent token callbacks must enforce the one-year lifetime advertised to T3,
and may revoke tokens sooner. The in-memory fallback enforces the same lifetime
but loses pairings when the gateway process stops.

## Live forwarding

`Config.Resolve` is the lifecycle seam: it starts a stopped machine, waits for
T3 readiness, and returns a `provider.Endpoint` plus a T3 service token. Each
request owns its endpoint and closes it after the response or socket ends.
Endpoints can use public TLS with provider authentication, or a private
`DialContext`, such as Namespace's authenticated SSH tunnel.

The resolver is canceled if the client disconnects or startup exceeds the
configured timeout. A successful connection does not inherit a fixed startup
deadline: long-running WebSockets and streaming responses can continue until
one peer disconnects. HTTP response headers and TLS setup have separate bounded
timeouts. There is no automatic retry of a forwarded write.

The gateway preserves binary frames and the T3 request/response messages. It
tracks only `server.getConfig` and `subscribeServerConfig`, removing the
`directEndpoints` fields from their responses. This keeps T3 from bypassing the
stable fleet origin using a machine's LAN or provider address. Unrelated user
data is not modified. The RPC socket requests protocol 2 without frame splitting.

HTTP requests are limited to 128 MiB, descriptor/session JSON to 4 MiB, and each
WebSocket message to 16 MiB. Cookies and caller-supplied forwarding headers do
not reach the VM. Provider endpoint query credentials cannot be overridden by
client query parameters. Provider transport errors are returned as generic
messages rather than endpoint URLs or credentials.

## Network exposure

The configured origin fixes the accepted Host and port. Local origins permit
loopback aliases at that same port. Browser requests must come from
`t3code://app` or the gateway's own origin; arbitrary browser origins are refused.
Requests without an Origin header still require their route's credential.

The fleet daemon should bind loopback by default. Hosting it elsewhere requires
TLS termination and an explicit external origin; the gateway does not provision
certificates or expose an unauthenticated management interface.

## Current scope

- Descriptor and authenticated API reads require a running T3 server. They can
  wake a stopped VM; the gateway does not yet provide the offline cache used by
  t3boat.
- Pairing, bearer authentication, RPC/media tickets, live HTTP, live WebSockets,
  startup cancellation, and endpoint cleanup have focused tests.
- Browser/device streams work while their machine is live. This first version
  does not distinguish passive stream retries from requests that should wake it.
- URL-only attachment download/upload credentials are not rewritten or cached.
  Routes requiring those flows need a T3 compatibility test before being claimed
  as supported. Normal bearer-authenticated API traffic is relayed.
- OAuth MCP linked-environment sessions and automatic provider login forwarding
  are separate features, not implemented by this gateway.

Use an image whose T3 version matches the client used in the compatibility test.
Do not copy an authenticated T3 database or provider login into a reusable image.
