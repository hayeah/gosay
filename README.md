# gosay

CLI tool that converts text to speech using the OpenAI TTS API. Streams PCM audio with adaptive buffering for gapless playback, and optionally saves to WAV.

## Install

```bash
go install github.com/hayeah/gosay@latest
```

Or build from source:

```bash
git clone https://github.com/hayeah/gosay.git
cd gosay
go build
```

## Requirements

- Go 1.22+
- `OPENAI_API_KEY` environment variable

## Usage

```bash
# Basic usage
gosay "Hello there"

# Choose a voice
gosay --voice nova "Hello"

# Pipe text from stdin
echo "some text" | gosay

# Save to WAV file (say-{voice}_YYYYMMDD_HHMMSS.wav)
gosay --save "Save this to a file"

# Combine flags
gosay -s -v coral -m tts-1-hd --speed 1.2 "High quality, slightly faster"
```

## Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--voice` | `-v` | `alloy` | Voice to use |
| `--model` | `-m` | `tts-1` | TTS model |
| `--speed` | | `1.0` | Playback speed (0.25–4.0) |
| `--save` | `-s` | `false` | Save to timestamped WAV file |
| `--no-replace` | | `false` | Disable text conversion |
| `--verbose` | | `false` | Debug output |

## Voices

alloy, ash, ballad, coral, echo, fable, nova, onyx, sage, shimmer

## Text Conversion

By default, gosay converts programmatic text into speech-friendly form:

- Digits become words: `fork2` → "fork two"
- `_` and `/` become words with pauses: `foo_bar` → "foo ... underscore ... bar"
- `@` and `#` become words: `test@example` → "test at example"
- Extra whitespace is collapsed

Use `--no-replace` to disable this and speak text as-is.

## How It Works

- Streams raw PCM audio (24kHz, 16-bit mono) from the OpenAI API
- An **adaptive buffer** decides when to start playback:
  - **Short text**: waits for the full response before playing (avoids streaming artifacts)
  - **Long text**: starts playing after 2 seconds of audio is buffered, with watermark-based pause/resume to prevent underruns
- Audio output via [oto](https://github.com/ebitengine/oto) (uses AudioToolbox on macOS, ALSA on Linux)
- File lock (`~/.say_audio_lock`) prevents overlapping playback from concurrent invocations
