# Third-party notices

## t3boat

The T3 pairing, gateway, and lifecycle design was studied from
[t3boat](https://github.com/paukraft/t3boat), by Pau Kraft. t3fleet is an
independent Go implementation. The upstream MIT notice is reproduced below to
preserve attribution for any protocol implementation material derived from that
work. The idle workload markers and RPC background classification were studied
from `machine/watchdog.mjs` and `proxy/classify.rs` at commit
`bd488c87c88b1b467709afaa3080d837483aafbb`.

```text
MIT License

Copyright (c) 2026 Pau Kraft

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Go dependencies

The module manifest and checksums are in `go.mod` and `go.sum`. Dependency source
retains its own license notices. Direct dependencies include:

- [coder/websocket](https://github.com/coder/websocket), ISC license.
- [golang.org/x/crypto](https://go.googlesource.com/crypto), Go BSD-style license.
- [Namespace integrations](https://github.com/namespacelabs/integrations), Apache-2.0 license.

T3 Code, Boat, Namespace, Claude Code, and Codex are separate products with their
own terms and licenses. This repository does not grant rights to their hosted
services or copy their credentials into a distributed image.
