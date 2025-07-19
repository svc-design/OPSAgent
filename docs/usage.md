# Usage

This repository only contains skeleton code. To build the binary run:

```bash
$ go build -o opsagent
```

Run the executable with an optional configuration file:

```bash
$ ./opsagent -config config.yaml
```

The configuration loader currently returns default values. Future versions will
parse real configuration files describing metric, trace and log sources.
