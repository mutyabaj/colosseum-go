package voice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"
)

const vitaSystemPrompt = `You are the voice assistant for Minnesota EquiVoice Partnership's free VITA (Volunteer Income Tax Assistance) tax preparation service. You answer incoming phone calls.

Key facts:
- VITA is FREE for households earning under $67,000/year
- You are IRS-certified and help with W-2s, 1099s, and most common tax situations
- Service days: Saturdays and Sundays, 9 AM to 5 PM Central Time
- Locations: First two Saturdays of each month at Saint Paul Public Library; last two Saturdays at Rondo Community Library
- Virtual appointments and document drop-off are also available
- Booking link is texted to callers automatically when they call

Eligibility questions — answer directly, never ask for income:
- If someone asks whether they qualify, say: "VITA is free for households that earned under sixty-seven thousand dollars last year. If that sounds like you, we'd love to help."
- Never ask the caller how much they earn. Never ask for any dollar amounts.
- If someone says they are unsure whether they qualify, encourage them to book an appointment or speak with a volunteer.

Voice response rules — CRITICAL:
- Keep every response to 1-2 sentences maximum
- Ask only ONE question at a time
- Never read out long lists — offer to text details instead
- Speak numbers and times naturally: "nine AM" not "9:00 AM"
- Confirm important info: dates, addresses, appointment times
- If caller asks about Social Security numbers, bank accounts, or specific tax figures, say you cannot discuss those over the phone and offer to connect them with a volunteer

You can do three things:
1. Answer questions about VITA eligibility, locations, hours, documents needed
2. Offer to connect the caller with a human volunteer (say you're transferring them)
3. Take a voicemail message

Do NOT:
- Ask callers about their income, earnings, or financial details
- Collect sensitive financial data (SSNs, account numbers, income amounts)
- Make promises about refund amounts
- Discuss tax situations more complex than basic W-2/1099 filing`

// Message is a single turn in the conversation.
type Message struct {
	Role    string
	Content string
}

// ConversationResult is what the LLM produced for one turn.
type ConversationResult struct {
	Text           string
	RequestsHuman  bool // caller wants to speak with a volunteer
	RequestsVoicemail bool
}

// RespondToTranscript sends the caller's transcript to Claude and streams back
// the reply text in sentence-sized phrases via the returned channel.
// history is the conversation so far (modified in place to append this exchange).
func RespondToTranscript(ctx context.Context, history *[]Message, transcript string) (<-chan string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	*history = append(*history, Message{Role: "user", Content: transcript})

	messages := make([]map[string]string, len(*history))
	for i, m := range *history {
		messages[i] = map[string]string{"role": m.Role, "content": m.Content}
	}

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 150,
		"system":     vitaSystemPrompt,
		"messages":   messages,
		"stream":     true,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(b))
	}

	phrases := make(chan string, 8)
	go func() {
		defer close(phrases)
		defer resp.Body.Close()

		var fullText strings.Builder
		var buf strings.Builder

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var event struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			if event.Type != "content_block_delta" || event.Delta.Type != "text_delta" {
				continue
			}

			token := event.Delta.Text
			buf.WriteString(token)
			fullText.WriteString(token)

			// Emit phrase when we hit sentence-ending punctuation followed by a space or end
			current := buf.String()
			if phrase, rest, ok := extractPhrase(current); ok {
				select {
				case phrases <- strings.TrimSpace(phrase):
				case <-ctx.Done():
					return
				}
				buf.Reset()
				buf.WriteString(rest)
			}
		}

		// Emit any remaining text
		if remaining := strings.TrimSpace(buf.String()); remaining != "" {
			select {
			case phrases <- remaining:
			case <-ctx.Done():
			}
		}

		// Store assistant reply in history
		if text := strings.TrimSpace(fullText.String()); text != "" {
			*history = append(*history, Message{Role: "assistant", Content: text})
		}
	}()

	return phrases, nil
}

// extractPhrase splits off a complete sentence from s.
// Returns (phrase, remainder, true) when a phrase boundary is found.
func extractPhrase(s string) (phrase, rest string, ok bool) {
	for i, ch := range s {
		if ch == '.' || ch == '?' || ch == '!' {
			// Check next character is whitespace or end of string
			next := i + 1
			if next >= len(s) || unicode.IsSpace(rune(s[next])) {
				return s[:next], s[next:], true
			}
		}
		// Also split on comma + space when phrase is long enough (natural breathing point)
		if ch == ',' && i > 40 && i+1 < len(s) && s[i+1] == ' ' {
			return s[:i+1], s[i+1:], true
		}
	}
	return "", s, false
}

// RequestsTransfer returns true if the reply text indicates the caller wants a human.
func RequestsTransfer(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range []string{
		"transfer", "connect you", "connect you with a volunteer",
		"human volunteer", "speak with someone", "real person",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// RequestsVoicemail returns true if the reply text indicates voicemail should be taken.
func RequestsVoicemail(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "leave a message") || strings.Contains(lower, "voicemail")
}
