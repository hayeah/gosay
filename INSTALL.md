# gosay

OpenAI TTS CLI tool in Go.

## Install

```bash
go install github.com/hayeah/gosay@latest
```

Or build locally:

```bash
git clone https://github.com/hayeah/gosay.git
cd gosay
go build
```

## Requirements

- `OPENAI_API_KEY` environment variable (or in `.env` file)

## Usage

```bash
gosay "Hello there"
gosay --voice nova "Hello"
gosay --save -v nova "Save this"
echo "piped text" | gosay
gosay --no-replace "fork2_002"
gosay --verbose "debug test"
```
