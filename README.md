# t3fleet

Run T3 Code agents on persistent cloud machines, using Boat or Namespace.

`t3fleet` manages machine lifecycle and presents a stable, authenticated T3 endpoint. Agents execute on the remote machine. Provider adapters use native APIs, without requiring their CLIs at runtime.

This repository is under active initial development. The first milestone implements a local daemon and CLI; this README will describe the exact supported workflow as it lands.

## Development

The toolchain is pinned with Nix (Go 1.27.3):

```sh
nix develop
make lint
make test
```

The code is separated into the public cloud-provider contract in `pkg/provider`, vendor adapters below it, and internal orchestration, persistence, and T3 protocol modules.

## License

MIT. See [LICENSE](LICENSE). The architecture is inspired by [t3boat](https://github.com/paukraft/t3boat); this is an independent project.
