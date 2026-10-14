package opl2

import (
	"math"
	"testing"
)

// writeNow writes registers and runs the chip until it has taken them.
func writeNow(c *Chip, regs ...int) {
	for i := 0; i+1 < len(regs); i += 2 {
		c.Write(regs[i], byte(regs[i+1]))
	}
	for range len(regs) / 2 {
		c.Sample()
	}
}

// sineVoice sets channel ch up as a plain sine on its carrier, loud, with instant
// attack and full sustain.
func sineVoice(c *Chip, ch int) {
	mod, car := ch%3+ch/3*8, ch%3+ch/3*8+3
	writeNow(c,
		RegOpFlags+mod, 1, RegOpFlags+car, 1+1<<5,
		RegOpLevel+mod, 63, RegOpLevel+car, 0,
		RegOpAttackDecay+mod, 15*16, RegOpAttackDecay+car, 15*16,
		RegOpSustainRelease+mod, 15, RegOpSustainRelease+car, 15,
		RegFeedback+ch, 0)
}

// keyOn keys channel ch at frequency number fnum in block.
func keyOn(c *Chip, ch, fnum, block int) {
	writeNow(c, RegFreqLow+ch, fnum%256, RegFreqHigh+ch, 1<<5|block<<2|fnum/256)
}

func TestPitch(t *testing.T) {
	c := New()
	sineVoice(c, 0)
	// f = fnum * Rate / 2^(20-block): 440 Hz is fnum 580 in block 4
	keyOn(c, 0, 580, 4)
	crossings, prev := 0, 0
	for range Rate {
		v := c.Sample()
		if prev < 0 && v >= 0 {
			crossings++
		}
		prev = v
	}
	if math.Abs(float64(crossings)-440) > 2 {
		t.Errorf("a second of fnum 580 block 4 has %d periods, want 440", crossings)
	}
}

func TestKeyOffOnRestarts(t *testing.T) {
	c := New()
	sineVoice(c, 0)
	keyOn(c, 0, 580, 4)
	for range 1000 {
		c.Sample()
	}
	// key off and on again at once: the chip takes them in different samples, so the
	// envelope goes to release and starts a new attack
	c.Write(RegFreqHigh, 4<<2|2)
	c.Write(RegFreqHigh, 1<<5|4<<2|2)
	c.Sample()
	if s := c.op[3].stage; s != release {
		t.Fatalf("after the key off the carrier is in stage %d, want release", s)
	}
	c.Sample()
	c.Sample()
	if s := c.op[3].stage; s != attack && s != decay && s != sustain {
		t.Fatalf("after the key on the carrier is in stage %d, want a new attack", s)
	}
}

func TestSilent(t *testing.T) {
	c := New()
	if !c.Silent(0) {
		t.Fatal("a channel never keyed is not silent")
	}
	sineVoice(c, 0)
	keyOn(c, 0, 580, 4)
	if c.Silent(0) {
		t.Fatal("a keyed channel is silent")
	}
	writeNow(c, RegFreqHigh, 4<<2|2)
	for range Rate {
		c.Sample()
	}
	if !c.Silent(0) {
		t.Fatal("a released channel with the fastest release rate still sounds after a second")
	}
}

// energy is the sum of squares of n samples.
func energy(c *Chip, n int) float64 {
	e := 0.0
	for range n {
		v := float64(c.Sample())
		e += v * v
	}
	return e
}

// drumVoices sets up the drum channels as sines and gives them frequencies.
func drumVoices(c *Chip) {
	for ch := drumFirst; ch < Channels; ch++ {
		sineVoice(c, ch)
		mod := ch%3 + ch/3*8
		writeNow(c, RegOpLevel+mod, 0, RegFreqLow+ch, 0, RegFreqHigh+ch, 2<<2|1)
	}
}

func TestRhythmDrumsSound(t *testing.T) {
	for _, d := range []struct {
		name string
		key  int
	}{{"bass drum", DrumBass}, {"snare", DrumSnare}, {"tom", DrumTom}, {"cymbal", DrumCymbal}, {"hi-hat", DrumHiHat}} {
		c := New()
		writeNow(c, RegTest, 1<<5)
		drumVoices(c)
		writeNow(c, RegRhythm, RhythmMode)
		// a silent operator in its negative half puts out -1 (the chip's one's complement)
		if e := energy(c, 2000); e > 100*2000 {
			t.Errorf("%s: rhythm mode with no drum keyed sounds (%g)", d.name, e)
		}
		writeNow(c, RegRhythm, RhythmMode|d.key)
		if e := energy(c, 2000); e < 1e6 {
			t.Errorf("%s: keyed but silent", d.name)
		}
	}
}

func TestRhythmNoise(t *testing.T) {
	// the snare's phase is made of the noise: two chips that differ only in the noise
	// generator's state play different snares
	a, b := New(), New()
	b.noise = 12345
	var sa, sb []int
	for _, c := range []*Chip{a, b} {
		drumVoices(c)
		writeNow(c, RegRhythm, RhythmMode|DrumSnare)
	}
	for range 2000 {
		sa, sb = append(sa, a.Sample()), append(sb, b.Sample())
	}
	same := true
	for i := range sa {
		same = same && sa[i] == sb[i]
	}
	if same {
		t.Error("the snare does not depend on the noise")
	}
}

func TestRhythmOffReleasesDrums(t *testing.T) {
	c := New()
	drumVoices(c)
	writeNow(c, RegRhythm, RhythmMode|DrumBass|DrumSnare|DrumTom|DrumCymbal|DrumHiHat)
	writeNow(c, RegRhythm, 0)
	for s := 12; s < slots; s++ {
		if c.op[s].keys&keyedByDrum != 0 {
			t.Errorf("slot %d still keyed as a drum out of rhythm mode", s)
		}
	}
}

func TestSampleChannelsSum(t *testing.T) {
	a, b := New(), New()
	for _, c := range []*Chip{a, b} {
		for ch := range Channels {
			sineVoice(c, ch)
			keyOn(c, ch, 300+40*ch, 3)
		}
	}
	var out [Channels]int
	for range 5000 {
		b.SampleChannels(&out)
		sum := 0
		for _, v := range out {
			sum += v
		}
		if got := a.Sample(); got != max(-32768, min(32767, sum)) {
			t.Fatalf("Sample %d, SampleChannels sum %d", got, sum)
		}
	}
}

func TestResampler(t *testing.T) {
	c := New()
	sineVoice(c, 0)
	keyOn(c, 0, 580, 4)
	r := NewResampler(c, 44100)
	out := make([]int16, 44100)
	r.Generate(out)
	crossings := 0
	for i := 1; i < len(out); i++ {
		if out[i-1] < 0 && out[i] >= 0 {
			crossings++
		}
	}
	if math.Abs(float64(crossings)-440) > 2 {
		t.Errorf("a resampled second has %d periods, want 440", crossings)
	}
}

func TestReg(t *testing.T) {
	c := New()
	c.Write(RegFreqHigh+3, 42)
	if c.Reg(RegFreqHigh+3) != 42 {
		t.Error("Reg does not read back a pending write")
	}
}

func TestResamplerMix(t *testing.T) {
	a, b := New(), New()
	for _, c := range []*Chip{a, b} {
		for ch := range 2 {
			sineVoice(c, ch)
			keyOn(c, ch, 400+100*ch, 4)
		}
	}
	plain := NewResampler(a, 44100)
	mixed := NewResampler(b, 44100)
	mixed.Mix = func(ch *[Channels]int) int { return ch[0] } // channel 1 muted
	only0 := 0
	for range 4410 {
		p, m := plain.Next(), mixed.Next()
		if p != m {
			only0++
		}
	}
	if only0 < 1000 {
		t.Errorf("muting a channel changed only %d of 4410 samples", only0)
	}
}
