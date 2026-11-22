# T3 protocol gateway

`t3fleet` exposes one stable HTTP origin per machine. T3 Code pairs with that
origin rather than the provider's hostname. This page describes the direct worker gateway used for recovery. The normal [fleet endpoint](dispatch.md) pairs once and routes projects and threads across workers.

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

The fleet daemon binds loopback. A TLS proxy can expose the T3 listener on a
private network when `dispatch.externalOrigin` names its HTTPS origin. The
gateway selects the origin from the validated `Host`, never from forwarded
headers. Browser origins must match the selected endpoint or be
`t3code://app`. OAuth discovery and peer requests use the selected authority.
The gateway does not provision certificates or expose the administration API
on that endpoint.

## Current scope

Direct worker pairing, bearer authentication, RPC/media tickets, HTTP and WebSocket forwarding, and connection cleanup have local tests. Public discovery reads do not wake a worker. There is no offline chat cache, forwarding of model logins, or credential rewriting for URL-only attachments. The fleet gateway additionally handles [Linked Environments](dispatch.md#launch-from-another-t3-server).
