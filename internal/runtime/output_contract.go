package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const maxContractInputBytes = 256 * 1024

var artifactContentURLPattern = regexp.MustCompile(`(?i)/api/runs/[^/\s)]+/artifacts/[^/\s)]+/content`)

type mediaEvidence struct {
	Images int
	Videos int
	Audios int
}

type mediaClaim struct {
	ClaimsDelivery bool
	MentionsImage  bool
	MentionsVideo  bool
	MentionsAudio  bool
}

func validateOutputContract(contractType, payload, output string) (bool, string) {
	contractType = normalizeContractType(contractType)
	output = strings.TrimSpace(output)
	if contractType == "none" {
		return true, "no contract"
	}
	if len(output) > maxContractInputBytes {
		return false, fmt.Sprintf("output exceeds %d bytes validation cap", maxContractInputBytes)
	}
	switch contractType {
	case "regex":
		re, err := regexp.Compile(payload)
		if err != nil {
			return false, fmt.Sprintf("invalid regex contract: %v", err)
		}
		if !re.MatchString(output) {
			return false, "regex did not match output"
		}
		return true, "regex matched"
	case "json_schema":
		var schema map[string]any
		if err := json.Unmarshal([]byte(payload), &schema); err != nil {
			return false, fmt.Sprintf("invalid json schema payload: %v", err)
		}
		var value any
		if err := json.Unmarshal([]byte(output), &value); err != nil {
			return false, fmt.Sprintf("output is not valid JSON: %v", err)
		}
		if err := validateJSONSchemaSubset(schema, value, "$"); err != nil {
			return false, err.Error()
		}
		return true, "json schema passed"
	default:
		return false, fmt.Sprintf("unsupported contract type: %s", contractType)
	}
}

func validateJSONSchemaSubset(schema map[string]any, value any, path string) error {
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s expected object", path)
		}
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, item := range reqRaw {
				key, _ := item.(string)
				if key == "" {
					continue
				}
				if _, exists := obj[key]; !exists {
					return fmt.Errorf("%s missing required field %q", path, key)
				}
			}
		}
		if propsRaw, ok := schema["properties"].(map[string]any); ok {
			for key, def := range propsRaw {
				childSchema, _ := def.(map[string]any)
				if childSchema == nil {
					continue
				}
				if childValue, exists := obj[key]; exists {
					if err := validateJSONSchemaSubset(childSchema, childValue, path+"."+key); err != nil {
						return err
					}
				}
			}
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s expected array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s expected string", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s expected number", path)
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("%s expected integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s expected boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s expected null", path)
		}
	case "":
		// no type constraint
	default:
		return fmt.Errorf("%s unsupported schema type %q", path, typeName)
	}
	return nil
}

func normalizeContractType(raw string) string {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	if normalized == "" {
		return "none"
	}
	return normalized
}

func truncateForEvent(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen-1] + "..."
}

func validateProvenanceOutputContract(output string, media mediaEvidence) (bool, string) {
	normalized := strings.ToLower(strings.TrimSpace(output))
	if normalized == "" {
		return true, "empty output"
	}
	claim := parseMediaClaim(normalized)
	if claim.ClaimsDelivery && claim.MentionsImage && media.Images == 0 {
		return false, "response claims screenshot/image attachment but no image artifact exists"
	}
	if claim.ClaimsDelivery && claim.MentionsVideo && media.Videos == 0 {
		return false, "response claims video attachment but no video artifact exists"
	}
	if claim.ClaimsDelivery && claim.MentionsAudio && media.Audios == 0 {
		return false, "response claims audio attachment but no audio artifact exists"
	}
	if claim.ClaimsDelivery && (claim.MentionsImage || claim.MentionsVideo || claim.MentionsAudio) && !hasArtifactContentLink(output) {
		return false, "response claims attachment but does not include an artifact content link"
	}
	return true, "provenance media contract passed"
}

var (
	imageClaimPattern = regexp.MustCompile(`(?i)\b(screenshot|captured image|image attached|photo attached|image below|screenshot below|attached image|attached photo)\b`)
	videoClaimPattern = regexp.MustCompile(`(?i)\b(video attached|attached video|video below|recorded video)\b`)
	audioClaimPattern = regexp.MustCompile(`(?i)\b(audio attached|attached audio|audio below|recorded audio)\b`)
	deliveryPattern   = regexp.MustCompile(`(?i)\b(attached|attachment|included below|here is the (screenshot|image|photo|video|audio)|here's the (screenshot|image|photo|video|audio))\b`)
)

func parseMediaClaim(normalizedOutput string) mediaClaim {
	return mediaClaim{
		MentionsImage:  imageClaimPattern.MatchString(normalizedOutput),
		MentionsVideo:  videoClaimPattern.MatchString(normalizedOutput),
		MentionsAudio:  audioClaimPattern.MatchString(normalizedOutput),
		ClaimsDelivery: deliveryPattern.MatchString(normalizedOutput),
	}
}

func hasArtifactContentLink(output string) bool {
	return artifactContentURLPattern.MatchString(output)
}
