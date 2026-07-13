package voice

// EncodeMulaw converts 16-bit linear PCM samples to 8-bit µ-law.
// Twilio Media Streams expect µ-law encoded audio at 8000 Hz.
func EncodeMulaw(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = encodeMulawSample(s)
	}
	return out
}

// DecodeMulaw converts 8-bit µ-law bytes to 16-bit linear PCM samples.
func DecodeMulaw(mu []byte) []int16 {
	out := make([]int16, len(mu))
	for i, b := range mu {
		out[i] = decodeMulawSample(b)
	}
	return out
}

func encodeMulawSample(s int16) byte {
	const bias = 0x84
	const clip = 32635

	sign := 0
	if s < 0 {
		sign = 0x80
		s = -s
	}
	if int(s) > clip {
		s = clip
	}
	s += bias

	exp := 7
	for ; exp > 0 && (int(s)&(1<<(exp+3))) == 0; exp-- {
	}
	mantissa := (int(s) >> (exp + 3)) & 0x0F
	return byte(^(sign | (exp << 4) | mantissa))
}

func decodeMulawSample(mu byte) int16 {
	mu = ^mu
	sign := mu & 0x80
	exp := int((mu >> 4) & 0x07)
	mantissa := int(mu & 0x0F)
	magnitude := ((mantissa << 3) + 0x84) << uint(exp+1)
	if sign != 0 {
		return int16(0x84 - magnitude)
	}
	return int16(magnitude - 0x84)
}

// PCM16ToBytes converts int16 PCM samples to little-endian byte slice.
func PCM16ToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		out[i*2] = byte(s)
		out[i*2+1] = byte(uint16(s) >> 8)
	}
	return out
}

// BytesToPCM16 converts little-endian bytes to int16 PCM samples.
func BytesToPCM16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(uint16(b[i*2]) | uint16(b[i*2+1])<<8)
	}
	return out
}
