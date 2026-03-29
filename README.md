# koolbase-cli

The official CLI for [Koolbase](https://koolbase.com) — deploy and manage your Koolbase functions from the terminal.

## Installation

### macOS / Linux
```bash
curl -fsSL https://raw.githubusercontent.com/kennedyowusu/koolbase-cli/main/install.sh | sh
```

### Build from source
```bash
git clone https://github.com/kennedyowusu/koolbase-cli
cd koolbase-cli
go build -o koolbase .
mv koolbase /usr/local/bin/koolbase
```

## Usage

### Login
```bash
koolbase login
```

### Deploy a function
```bash
# TypeScript (Deno) — auto-detected from .ts extension
koolbase deploy send-email --file ./functions/send_email.ts --project <project_id>

# Dart — auto-detected from .dart extension
koolbase deploy process-order --file ./functions/process.dart --project <project_id>
```

### List functions
```bash
koolbase functions list --project <project_id>
```

### Invoke a function
```bash
koolbase invoke send-email --project <project_id>
koolbase invoke send-email --project <project_id> --data '{"email":"user@example.com"}'
```

### View logs
```bash
koolbase logs send-email --project <project_id>
koolbase logs send-email --project <project_id> --limit 50
```

## Documentation

Full docs at [docs.koolbase.com](https://docs.koolbase.com)

## License

MIT
