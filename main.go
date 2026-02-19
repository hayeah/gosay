package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/ebitengine/oto/v3"
	"github.com/joho/godotenv"
	flag "github.com/spf13/pflag"
)

// Audio constants
const (
	sampleRate    = 24000
	channels      = 1
	bytesPerSample = 2 // 16-bit
	bytesPerSec   = sampleRate * channels * bytesPerSample // 48000

	// AdaptiveBuffer thresholds
	initialThreshold = 2 * bytesPerSec          // 2s = 96KB before starting playback
	lowWatermark     = bytesPerSec / 2           // 0.5s = 24KB
	highWatermark    = 3 * bytesPerSec / 2       // 1.5s = 72KB
)

var validVoices = []string{"alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer"}
var validModels = []string{"tts-1", "tts-1-hd"}

// Character-to-word mappings
var charToWord = map[rune]string{
	'_': "underscore",
	'/': "slash",
	'@': "at",
	'#': "hash",
}

// Characters that get pauses around them
var pauseChars = map[rune]bool{
	'_': true,
	'/': true,
}

var digitToWord = map[rune]string{
	'0': "zero",
	'1': "one",
	'2': "two",
	'3': "three",
	'4': "four",
	'5': "five",
	'6': "six",
	'7': "seven",
	'8': "eight",
	'9': "nine",
}

func convertTextForSpeech(text string, disableReplacements bool) string {
	if disableReplacements {
		return text
	}

	runes := []rune(text)
	var result []string
	i := 0

	for i < len(runes) {
		ch := runes[i]

		if unicode.IsDigit(ch) {
			// Add space before digits if previous char was not a separator
			if len(result) > 0 {
				last := result[len(result)-1]
				if last != " " && last != "." && last != "!" && last != "?" && last != "," && last != ";" && last != ":" {
					result = append(result, " ")
				}
			}

			// Collect consecutive digits
			j := i
			for j < len(runes) && unicode.IsDigit(runes[j]) {
				j++
			}

			// Convert each digit
			for k := i; k < j; k++ {
				if k > i {
					result = append(result, " ")
				}
				result = append(result, digitToWord[runes[k]])
			}

			// Add space after digits if next char is alphanumeric
			if j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) {
				result = append(result, " ")
			}

			i = j
		} else if word, ok := charToWord[ch]; ok {
			if pauseChars[ch] {
				result = append(result, " ... ", word, " ... ")
			} else {
				result = append(result, " ", word, " ")
			}
			i++
		} else {
			result = append(result, string(ch))
			i++
		}
	}

	// Clean up extra spaces
	joined := strings.Join(result, "")
	return strings.Join(strings.Fields(joined), " ")
}

// AdaptiveBuffer implements io.Reader with adaptive buffering for streaming audio.
// For short snippets, it waits for the source to complete before allowing reads.
// For long passages, it starts yielding data after an initial threshold is met.
type AdaptiveBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	readPos  int
	done     bool // source finished writing
	started  bool // playback has been allowed to start
	paused   bool // watermark-based pause active
	verbose  bool
}

func NewAdaptiveBuffer(verbose bool) *AdaptiveBuffer {
	ab := &AdaptiveBuffer{verbose: verbose}
	ab.cond = sync.NewCond(&ab.mu)
	return ab
}

// Fill runs in a background goroutine, reading from src into the buffer.
func (ab *AdaptiveBuffer) Fill(src io.Reader) {
	tmp := make([]byte, 8192)
	for {
		n, err := src.Read(tmp)
		if n > 0 {
			ab.mu.Lock()
			ab.buf = append(ab.buf, tmp[:n]...)
			buffered := len(ab.buf) - ab.readPos

			// Check if we should start playback
			if !ab.started && buffered >= initialThreshold {
				ab.started = true
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] Initial threshold reached (%d bytes), starting playback\n", buffered)
				}
			}

			// Resume from pause if above high watermark
			if ab.paused && buffered >= highWatermark {
				ab.paused = false
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] High watermark reached (%d bytes), resuming\n", buffered)
				}
			}

			ab.cond.Broadcast()
			ab.mu.Unlock()
		}
		if err != nil {
			ab.mu.Lock()
			ab.done = true
			// If source is done and we never hit threshold, start now
			if !ab.started {
				ab.started = true
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] Source complete (%d bytes total), starting playback\n", len(ab.buf)-ab.readPos)
				}
			}
			ab.cond.Broadcast()
			ab.mu.Unlock()
			return
		}
	}
}

// Read implements io.Reader for the audio player.
func (ab *AdaptiveBuffer) Read(p []byte) (int, error) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for {
		buffered := len(ab.buf) - ab.readPos

		// If not started yet, wait
		if !ab.started {
			ab.cond.Wait()
			continue
		}

		// If paused (below low watermark, waiting for high), wait unless done
		if ab.paused && !ab.done && buffered < highWatermark {
			ab.cond.Wait()
			continue
		}

		// Data available
		if buffered > 0 {
			n := copy(p, ab.buf[ab.readPos:])
			ab.readPos += n

			// Check low watermark for pause (only if source isn't done)
			newBuffered := len(ab.buf) - ab.readPos
			if !ab.done && !ab.paused && newBuffered < lowWatermark {
				ab.paused = true
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] Below low watermark (%d bytes), pausing\n", newBuffered)
				}
			}

			return n, nil
		}

		// No data and source is done
		if ab.done {
			return 0, io.EOF
		}

		// No data yet, wait
		ab.cond.Wait()
	}
}

// eofSignaler wraps an io.Reader and signals a channel when EOF is reached.
type eofSignaler struct {
	r    io.Reader
	done chan struct{}
	once sync.Once
}

func newEOFSignaler(r io.Reader) *eofSignaler {
	return &eofSignaler{r: r, done: make(chan struct{})}
}

func (e *eofSignaler) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.once.Do(func() { close(e.done) })
	}
	return n, err
}

// acquireLock acquires an exclusive file lock for audio playback.
func acquireLock(verbose bool) (int, error) {
	lockPath := filepath.Join(os.Getenv("HOME"), ".say_audio_lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_WRONLY, 0644)
	if err != nil {
		return -1, fmt.Errorf("open lock file: %w", err)
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Acquiring playback lock\n")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("acquire lock: %w", err)
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Lock acquired\n")
	}
	return fd, nil
}

func releaseLock(fd int, verbose bool) {
	if fd < 0 {
		return
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Releasing playback lock\n")
	}
	syscall.Flock(fd, syscall.LOCK_UN)
	syscall.Close(fd)
	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Lock released\n")
	}
}

// writeWAV writes PCM data as a WAV file.
func writeWAV(path string, pcmData []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dataLen := uint32(len(pcmData))
	fileLen := 36 + dataLen // header is 44 bytes, minus 8 for RIFF+size

	// RIFF header
	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, fileLen)
	f.Write([]byte("WAVE"))

	// fmt subchunk
	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16))      // subchunk size
	binary.Write(f, binary.LittleEndian, uint16(1))        // PCM format
	binary.Write(f, binary.LittleEndian, uint16(channels)) // channels
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	binary.Write(f, binary.LittleEndian, uint32(bytesPerSec))                    // byte rate
	binary.Write(f, binary.LittleEndian, uint16(channels*bytesPerSample))         // block align
	binary.Write(f, binary.LittleEndian, uint16(bytesPerSample*8))               // bits per sample

	// data subchunk
	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, dataLen)
	_, err = f.Write(pcmData)
	return err
}

type ttsRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	Speed          float64 `json:"speed"`
	ResponseFormat string  `json:"response_format"`
}

func streamTTS(apiKey, model, voice, text string, speed float64, verbose bool) (io.ReadCloser, error) {
	reqBody := ttsRequest{
		Model:          model,
		Input:          text,
		Voice:          voice,
		Speed:          speed,
		ResponseFormat: "pcm",
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] POST https://api.openai.com/v1/audio/speech\n")
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func main() {
	// CLI flags
	voice := flag.StringP("voice", "v", "alloy", "Voice: "+strings.Join(validVoices, ", "))
	model := flag.StringP("model", "m", "tts-1", "Model: "+strings.Join(validModels, ", "))
	speed := flag.Float64("speed", 1.0, "Speed (0.25–4.0)")
	save := flag.BoolP("save", "s", false, "Save to timestamped WAV file")
	noReplace := flag.Bool("no-replace", false, "Disable character→word conversion")
	verbose := flag.Bool("verbose", false, "Debug output")
	flag.Parse()

	// Load .env
	godotenv.Load()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY environment variable is not set")
		os.Exit(1)
	}

	// Validate parameters
	if !contains(validVoices, *voice) {
		fmt.Fprintf(os.Stderr, "Error: Invalid voice '%s'. Valid voices: %s\n", *voice, strings.Join(validVoices, ", "))
		os.Exit(1)
	}
	if !contains(validModels, *model) {
		fmt.Fprintf(os.Stderr, "Error: Invalid model '%s'. Valid models: %s\n", *model, strings.Join(validModels, ", "))
		os.Exit(1)
	}
	if *speed < 0.25 || *speed > 4.0 {
		fmt.Fprintln(os.Stderr, "Error: Speed must be between 0.25 and 4.0")
		os.Exit(1)
	}

	// Get text from positional arg or stdin
	var text string
	if flag.NArg() > 0 {
		text = strings.Join(flag.Args(), " ")
	} else {
		// Check if stdin is piped
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
				os.Exit(1)
			}
			text = strings.TrimSpace(string(data))
		} else {
			fmt.Fprintln(os.Stderr, "Error: No text provided. Pass text as an argument or pipe via stdin.")
			os.Exit(1)
		}
	}

	if text == "" {
		fmt.Fprintln(os.Stderr, "Error: Empty text")
		os.Exit(1)
	}

	// Add period if text doesn't end with punctuation
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if last != '.' && last != '?' && last != '!' {
			text = trimmed + "."
		}
	}

	// Convert text
	converted := convertTextForSpeech(text, *noReplace)
	if !*noReplace && converted != text {
		fmt.Fprintf(os.Stderr, "Converted text: %s\n", converted)
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Text: %s\n", converted)
		fmt.Fprintf(os.Stderr, "[DEBUG] Voice: %s, Model: %s, Speed: %.1f\n", *voice, *model, *speed)
	}

	// Acquire file lock
	lockFD, err := acquireLock(*verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer releaseLock(lockFD, *verbose)

	// Stream from OpenAI
	body, err := streamTTS(apiKey, *model, *voice, converted, *speed, *verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer body.Close()

	// Set up adaptive buffer
	adaptBuf := NewAdaptiveBuffer(*verbose)

	// If saving, tee the source into a capture buffer
	var pcmCapture *bytes.Buffer
	var src io.Reader = body
	if *save {
		pcmCapture = &bytes.Buffer{}
		src = io.TeeReader(body, pcmCapture)
	}

	// Start filling the buffer in background
	go adaptBuf.Fill(src)

	// Set up audio output with oto
	otoCtx, readyChan, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   sampleRate,
		ChannelCount: channels,
		Format:       oto.FormatSignedInt16LE,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing audio: %v\n", err)
		os.Exit(1)
	}
	<-readyChan

	// Wrap buffer in EOF signaler
	eof := newEOFSignaler(adaptBuf)
	player := otoCtx.NewPlayer(eof)

	fmt.Fprintln(os.Stderr, "Playing audio stream...")

	// Start playback — oto pulls data via Read calls
	player.Play()

	// Wait for EOF from the adaptive buffer
	<-eof.done

	// Drain remaining buffered audio in the player
	for player.BufferedSize() > 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Small delay to let the audio backend finish flushing to hardware
	time.Sleep(300 * time.Millisecond)

	if err := player.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Error closing player: %v\n", err)
	}

	// Save WAV if requested
	if *save && pcmCapture != nil {
		timestamp := time.Now().Format("20060102_150405")
		filename := fmt.Sprintf("say-%s_%s.wav", *voice, timestamp)
		fmt.Fprintf(os.Stderr, "Saving audio to %s...\n", filename)
		if err := writeWAV(filename, pcmCapture.Bytes()); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving WAV: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Audio saved to: %s\n", filename)
	}
}
