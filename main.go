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
	flag "github.com/spf13/pflag"
)

// Audio constants
const (
	sampleRate     = 24000
	channels       = 1
	bytesPerSample = 2 // 16-bit
	bytesPerSec    = sampleRate * channels * bytesPerSample // 48000

	// AdaptiveBuffer thresholds
	initialThreshold = 2 * bytesPerSec    // 2s = 96KB before starting playback
	lowWatermark     = bytesPerSec / 2     // 0.5s = 24KB
	highWatermark    = 3 * bytesPerSec / 2 // 1.5s = 72KB
)

var validVoices = []string{"alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer"}
var validModels = []string{"tts-1", "tts-1-hd"}

// --- Text conversion (pure functions) ---

var charToWord = map[rune]string{
	'_': "underscore",
	'/': "slash",
	'@': "at",
	'#': "hash",
}

var pauseChars = map[rune]bool{
	'_': true,
	'/': true,
}

var digitToWord = map[rune]string{
	'0': "zero", '1': "one", '2': "two", '3': "three", '4': "four",
	'5': "five", '6': "six", '7': "seven", '8': "eight", '9': "nine",
}

func convertTextForSpeech(text string, disable bool) string {
	if disable {
		return text
	}

	runes := []rune(text)
	var result []string
	i := 0

	for i < len(runes) {
		ch := runes[i]

		if unicode.IsDigit(ch) {
			if len(result) > 0 {
				last := result[len(result)-1]
				if last != " " && last != "." && last != "!" && last != "?" && last != "," && last != ";" && last != ":" {
					result = append(result, " ")
				}
			}
			j := i
			for j < len(runes) && unicode.IsDigit(runes[j]) {
				j++
			}
			for k := i; k < j; k++ {
				if k > i {
					result = append(result, " ")
				}
				result = append(result, digitToWord[runes[k]])
			}
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

	joined := strings.Join(result, "")
	return strings.Join(strings.Fields(joined), " ")
}

// --- WAV writer (pure function) ---

func writeWAV(path string, pcmData []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dataLen := uint32(len(pcmData))
	fileLen := 36 + dataLen

	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, fileLen)
	f.Write([]byte("WAVE"))

	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16))
	binary.Write(f, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(f, binary.LittleEndian, uint16(channels))
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	binary.Write(f, binary.LittleEndian, uint32(bytesPerSec))
	binary.Write(f, binary.LittleEndian, uint16(channels*bytesPerSample))
	binary.Write(f, binary.LittleEndian, uint16(bytesPerSample*8))

	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, dataLen)
	_, err = f.Write(pcmData)
	return err
}

// --- AdaptiveBuffer ---

// AdaptiveBuffer implements io.Reader with adaptive buffering for streaming audio.
// Short snippets wait for source completion; long passages start after an initial threshold.
type AdaptiveBuffer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	readPos int
	done    bool
	started bool
	paused  bool
	verbose bool
}

func NewAdaptiveBuffer(verbose bool) *AdaptiveBuffer {
	ab := &AdaptiveBuffer{verbose: verbose}
	ab.cond = sync.NewCond(&ab.mu)
	return ab
}

func (ab *AdaptiveBuffer) Fill(src io.Reader) {
	tmp := make([]byte, 8192)
	for {
		n, err := src.Read(tmp)
		if n > 0 {
			ab.mu.Lock()
			ab.buf = append(ab.buf, tmp[:n]...)
			buffered := len(ab.buf) - ab.readPos

			if !ab.started && buffered >= initialThreshold {
				ab.started = true
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] Initial threshold reached (%d bytes), starting playback\n", buffered)
				}
			}
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

func (ab *AdaptiveBuffer) Read(p []byte) (int, error) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for {
		buffered := len(ab.buf) - ab.readPos

		if !ab.started {
			ab.cond.Wait()
			continue
		}
		if ab.paused && !ab.done && buffered < highWatermark {
			ab.cond.Wait()
			continue
		}
		if buffered > 0 {
			n := copy(p, ab.buf[ab.readPos:])
			ab.readPos += n
			newBuffered := len(ab.buf) - ab.readPos
			if !ab.done && !ab.paused && newBuffered < lowWatermark {
				ab.paused = true
				if ab.verbose {
					fmt.Fprintf(os.Stderr, "[AdaptiveBuffer] Below low watermark (%d bytes), pausing\n", newBuffered)
				}
			}
			return n, nil
		}
		if ab.done {
			return 0, io.EOF
		}
		ab.cond.Wait()
	}
}

// --- eofSignaler ---

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

// --- App ---

type App struct {
	Voice     string
	Model     string
	Speed     float64
	Save      bool
	NoReplace bool
	Verbose   bool
	APIKey    string
	Text      string

	lockFD int
}

func (a *App) ParseArgs() error {
	flag.StringVarP(&a.Voice, "voice", "v", "alloy", "Voice: "+strings.Join(validVoices, ", "))
	flag.StringVarP(&a.Model, "model", "m", "tts-1", "Model: "+strings.Join(validModels, ", "))
	flag.Float64Var(&a.Speed, "speed", 1.0, "Speed (0.25–4.0)")
	flag.BoolVarP(&a.Save, "save", "s", false, "Save to timestamped WAV file")
	flag.BoolVar(&a.NoReplace, "no-replace", false, "Disable character→word conversion")
	flag.BoolVar(&a.Verbose, "verbose", false, "Debug output")
	flag.Parse()

	a.APIKey = os.Getenv("OPENAI_API_KEY")
	if a.APIKey == "" {
		return fmt.Errorf("OPENAI_API_KEY environment variable is not set")
	}

	if !contains(validVoices, a.Voice) {
		return fmt.Errorf("invalid voice '%s'. Valid voices: %s", a.Voice, strings.Join(validVoices, ", "))
	}
	if !contains(validModels, a.Model) {
		return fmt.Errorf("invalid model '%s'. Valid models: %s", a.Model, strings.Join(validModels, ", "))
	}
	if a.Speed < 0.25 || a.Speed > 4.0 {
		return fmt.Errorf("speed must be between 0.25 and 4.0")
	}

	text, err := a.readText()
	if err != nil {
		return err
	}
	a.Text = a.prepareText(text)

	return nil
}

func (a *App) readText() (string, error) {
	if flag.NArg() > 0 {
		return strings.Join(flag.Args(), " "), nil
	}
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", fmt.Errorf("empty text from stdin")
		}
		return text, nil
	}
	return "", fmt.Errorf("no text provided. Pass text as an argument or pipe via stdin")
}

func (a *App) prepareText(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if last != '.' && last != '?' && last != '!' {
			trimmed += "."
		}
	}

	converted := convertTextForSpeech(trimmed, a.NoReplace)
	if !a.NoReplace && converted != trimmed {
		fmt.Fprintf(os.Stderr, "Converted text: %s\n", converted)
	}
	return converted
}

func (a *App) acquireLock() error {
	lockPath := filepath.Join(os.Getenv("HOME"), ".say_audio_lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Acquiring playback lock\n")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("acquire lock: %w", err)
	}
	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Lock acquired\n")
	}
	a.lockFD = fd
	return nil
}

func (a *App) releaseLock() {
	if a.lockFD == 0 {
		return
	}
	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Releasing playback lock\n")
	}
	syscall.Flock(a.lockFD, syscall.LOCK_UN)
	syscall.Close(a.lockFD)
	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Lock released\n")
	}
	a.lockFD = 0
}

type ttsRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	Speed          float64 `json:"speed"`
	ResponseFormat string  `json:"response_format"`
}

func (a *App) streamTTS() (io.ReadCloser, error) {
	reqBody := ttsRequest{
		Model:          a.Model,
		Input:          a.Text,
		Voice:          a.Voice,
		Speed:          a.Speed,
		ResponseFormat: "pcm",
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] POST https://api.openai.com/v1/audio/speech\n")
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/speech", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
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

func (a *App) play(body io.ReadCloser) error {
	defer body.Close()

	adaptBuf := NewAdaptiveBuffer(a.Verbose)

	var pcmCapture *bytes.Buffer
	var src io.Reader = body
	if a.Save {
		pcmCapture = &bytes.Buffer{}
		src = io.TeeReader(body, pcmCapture)
	}

	go adaptBuf.Fill(src)

	otoCtx, readyChan, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   sampleRate,
		ChannelCount: channels,
		Format:       oto.FormatSignedInt16LE,
	})
	if err != nil {
		return fmt.Errorf("initializing audio: %w", err)
	}
	<-readyChan

	eof := newEOFSignaler(adaptBuf)
	player := otoCtx.NewPlayer(eof)

	fmt.Fprintln(os.Stderr, "Playing audio stream...")
	player.Play()

	<-eof.done
	for player.BufferedSize() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	if err := player.Close(); err != nil {
		return fmt.Errorf("closing player: %w", err)
	}

	if a.Save && pcmCapture != nil {
		timestamp := time.Now().Format("20060102_150405")
		filename := fmt.Sprintf("say-%s_%s.wav", a.Voice, timestamp)
		fmt.Fprintf(os.Stderr, "Saving audio to %s...\n", filename)
		if err := writeWAV(filename, pcmCapture.Bytes()); err != nil {
			return fmt.Errorf("saving WAV: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Audio saved to: %s\n", filename)
	}

	return nil
}

func (a *App) Run() error {
	if err := a.ParseArgs(); err != nil {
		return err
	}

	if a.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Text: %s\n", a.Text)
		fmt.Fprintf(os.Stderr, "[DEBUG] Voice: %s, Model: %s, Speed: %.1f\n", a.Voice, a.Model, a.Speed)
	}

	if err := a.acquireLock(); err != nil {
		return err
	}
	defer a.releaseLock()

	body, err := a.streamTTS()
	if err != nil {
		return err
	}

	return a.play(body)
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
	if err := (&App{}).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
