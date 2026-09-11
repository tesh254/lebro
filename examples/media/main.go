// Media demonstrates direct generation, durable video resume, voice prompt input,
// streaming speech, and an agent with an image-generation tool. Calls are paid
// only when explicitly run with provider credentials. See docs/media.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tesh254/lebro"
	schema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/openai"
	"github.com/tesh254/lebro/openrouter"
	"github.com/tesh254/lebro/voice"
)

type backend interface {
	lebro.ImageGenerator
	lebro.VideoGenerator
	lebro.Transcriber
	lebro.StreamingTranscriber
	lebro.SpeechSynthesizer
	lebro.StreamingSpeechSynthesizer
}

type diskSink struct{ dir string }

func (s diskSink) PutMedia(ctx context.Context, o lebro.MediaOperation, a lebro.MediaAsset, r io.Reader) (lebro.MediaAsset, error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return a, err
	}
	f, err := os.CreateTemp(s.dir, ".partial-")
	if err != nil {
		return a, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = lebro.CopyMedia(ctx, f, r, 64<<20); err != nil {
		return a, err
	}
	if err = f.Sync(); err != nil {
		return a, err
	}
	if err = f.Close(); err != nil {
		return a, err
	}
	a.ID = rand.Text()
	a.Locator = a.ID
	err = os.Rename(f.Name(), filepath.Join(s.dir, a.ID))
	return a, err
}
func provider(name, model string) (backend, error) {
	if name == "openrouter" {
		return openrouter.NewMedia(openrouter.MediaConfig{APIKey: os.Getenv("OPENROUTER_API_KEY"), BaseURL: os.Getenv("OPENROUTER_BASE_URL"), Model: model})
	}
	if name == "openai" {
		return openai.NewMedia(openai.MediaConfig{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL"), Model: model})
	}
	return nil, errors.New("provider must be openai or openrouter")
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "image", "image, video, resume, transcribe, speak, speak-stream, live, agent, or voice")
	providerName := flag.String("provider", "openrouter", "openrouter or openai")
	model := flag.String("model", "", "explicit provider media model")
	chatModel := flag.String("chat-model", "", "chat model for agent/voice modes")
	prompt := flag.String("prompt", "A watercolor of Nairobi at sunrise", "prompt or speech text")
	input := flag.String("input", "", "audio file (WAV for transcribe/voice; raw PCM16 24kHz mono for live)")
	output := flag.String("output", "media-output", "application-owned output directory")
	operationID := flag.String("operation", "", "stable operation ID; required for resume")
	db := flag.String("db", "media-jobs.db", "SQLite video checkpoint database")
	flag.Parse()
	if *model == "" {
		return errors.New("-model is required; see docs/media.md for supported profiles")
	}
	if *mode == "resume" && *operationID == "" {
		return errors.New("-operation is required for resume")
	}
	if *operationID == "" {
		*operationID = rand.Text()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	op := lebro.MediaOperation{ID: *operationID}
	fmt.Println("operation:", op.ID)
	media, err := provider(*providerName, *model)
	if err != nil {
		return err
	}
	sink := diskSink{dir: *output}
	save := func(content lebro.MediaContent) error {
		a, e := lebro.SaveMediaAsset(ctx, op, content, sink, nil, 64<<20)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(a)
	}
	switch *mode {
	case "image":
		result, e := media.GenerateImages(ctx, lebro.ImageRequest{Operation: op, Prompt: *prompt})
		if e != nil {
			return e
		}
		for _, image := range result.Images {
			if e = save(image); e != nil {
				return e
			}
		}
		return nil
	case "video", "resume":
		store, e := lebro.NewSQLiteStore(*db)
		if e != nil {
			return e
		}
		defer store.Close()
		if e = store.Migrate(ctx); e != nil {
			return e
		}
		service, e := lebro.NewVideoService(lebro.VideoServiceConfig{Generator: media, Jobs: store.MediaJobs(), Attempts: store.ModelAttempts()})
		if e != nil {
			return e
		}
		if *mode == "video" {
			j, e := service.Submit(ctx, lebro.VideoRequest{Operation: op, Prompt: *prompt})
			if e != nil {
				return e
			}
			_ = json.NewEncoder(os.Stdout).Encode(j)
			fmt.Println("Resume with -mode resume -operation", op.ID)
		}
		_, e = service.Wait(ctx, op, lebro.MediaWaitOptions{Interval: 5 * time.Second, RetryBudget: 3, Jitter: 0.2})
		if e != nil {
			return e
		}
		content, e := service.Open(ctx, op, 0)
		if e != nil {
			return e
		}
		return save(content)
	case "transcribe", "voice":
		f, e := os.Open(*input)
		if e != nil {
			return e
		}
		defer f.Close()
		result, e := media.Transcribe(ctx, lebro.TranscriptionRequest{Operation: op, Audio: lebro.MediaContent{Asset: lebro.MediaAsset{Kind: lebro.MediaAudio, MIMEType: "audio/wav"}, Reader: f}})
		if e != nil {
			return e
		}
		if *mode == "transcribe" {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		// In a UI, present result.Text for editing before submitting the agent run.
		agent, e := chatAgent(*providerName, *chatModel, nil, nil)
		if e != nil {
			return e
		}
		reply, e := agent.Run(ctx, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: result.Text}}})
		if e != nil {
			return e
		}
		ttsModel := "gpt-4o-mini-tts"
		if *providerName == "openrouter" {
			ttsModel = "openai/" + ttsModel
		}
		tts, e := provider(*providerName, ttsModel)
		if e != nil {
			return e
		}
		op.ID += "-speech"
		speech, e := tts.SynthesizeSpeech(ctx, lebro.SpeechRequest{Operation: op, Text: voice.AssistantText(reply), Voice: "alloy"})
		if e != nil {
			return e
		}
		return save(speech.Audio)
	case "speak":
		// SynthesizeSpeech returns the same bounded reader used by StreamSpeech.
		speech, e := media.SynthesizeSpeech(ctx, lebro.SpeechRequest{Operation: op, Text: *prompt, Voice: "alloy", Format: "mp3"})
		if e != nil {
			return e
		}
		return save(speech.Audio)
	case "speak-stream":
		return streamSpeech(ctx, op, media, *prompt, sink)
	case "live":
		f, e := os.Open(*input)
		if e != nil {
			return e
		}
		defer f.Close()
		return media.StreamTranscription(ctx, lebro.LiveTranscriptionRequest{Operation: op}, func(ctx context.Context, b []byte) (int, error) {
			if e := ctx.Err(); e != nil {
				return 0, e
			}
			return f.Read(b)
		}, func(e lebro.TranscriptionEvent) error { return json.NewEncoder(os.Stdout).Encode(e) })
	case "agent":
		tool, e := lebro.NewImageTool(lebro.ImageToolConfig{Generator: media, Sink: sink})
		if e != nil {
			return e
		}
		registry, e := lebro.NewToolRegistry(schema.NewCompiler())
		if e != nil {
			return e
		}
		if e = registry.Register(tool); e != nil {
			return e
		}
		agent, e := chatAgent(*providerName, *chatModel, registry, []lebro.ToolID{tool.Definition().ID})
		if e != nil {
			return e
		}
		result, e := agent.Run(ctx, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: *prompt}}})
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	default:
		return errors.New("unknown mode")
	}
}
func streamSpeech(ctx context.Context, op lebro.MediaOperation, media backend, prompt string, sink diskSink) error {
	if err := os.MkdirAll(sink.dir, 0700); err != nil {
		return err
	}
	id := rand.Text()
	f, err := os.CreateTemp(sink.dir, ".partial-")
	if err != nil {
		return err
	}
	info, err := media.StreamSpeech(ctx, lebro.SpeechRequest{Operation: op, Text: prompt, Voice: "alloy", Format: "mp3"}, func(_ lebro.MediaAsset, b []byte) error {
		_, e := f.Write(b)
		return e
	})
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(sink.dir, id)); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	a := lebro.MediaAsset{ID: id, Locator: id, Kind: lebro.MediaAudio, MIMEType: "audio/mpeg", Codec: "mp3", Provider: info.Provider, Model: info.Model, ProviderRequestID: info.ProviderRequestID}
	return json.NewEncoder(os.Stdout).Encode(a)
}
func chatAgent(providerName, model string, tools *lebro.ToolRegistry, ids []lebro.ToolID) (*lebro.Agent, error) {
	if model == "" {
		return nil, errors.New("-chat-model is required")
	}
	key := os.Getenv("OPENAI_API_KEY")
	base := os.Getenv("OPENAI_BASE_URL")
	if providerName == "openrouter" {
		key = os.Getenv("OPENROUTER_API_KEY")
		base = "https://openrouter.ai/api/v1"
		if configured := os.Getenv("OPENROUTER_BASE_URL"); configured != "" {
			base = configured
		}
	}
	chat, err := openai.New(openai.Config{APIKey: key, Model: model, BaseURL: base})
	if err != nil {
		return nil, err
	}
	return lebro.NewAgent(lebro.AgentConfig{Definition: lebro.AgentDefinition{ID: "media-agent", Name: "Media agent", Instructions: "Use the image tool for image requests. Return asset IDs; never invent media URLs.", Tools: ids}, Tools: tools, Model: chat})
}
