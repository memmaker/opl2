// Package opl2 emulates the Yamaha YM3812 (OPL2), the FM chip of the AdLib and Sound
// Blaster cards, at the level of its integer arithmetic: the chip's log-sine and
// exponent tables, the 9-bit envelope generator stepped by a global rate counter, the
// 19-bit phase accumulators, the tremolo and vibrato counters, the four waveforms,
// feedback and both connections, and rhythm mode with its noise generator and the phase
// bits the hi-hat, snare and cymbal are made of. It follows the behaviour of the chip as
// the Nuked OPL3 research documents it (the OPL2 subset); no code is taken from it.
//
// The chip runs at its own rate (Rate). A program writes registers with Write and takes
// samples with Sample, or with SampleChannels to pan the channels itself; Resampler
// turns the output into another rate.
package opl2

import "math"

// Rate is the chip's sample rate: its 3.579545 MHz clock divided by 72.
const Rate = 49716

// Channels is the number of two-operator channels.
const Channels = 9

// Register numbers. The operator registers come in groups of 32 (OpGroup); the operator
// at offset o of a group is slot o/8*6 + o%8, for o%8 below 6. The channel registers
// come in groups of nine.
const (
	RegTest             = 1   // bit 5 enables the waveform select registers
	RegKeyboardSplit    = 8   // bit 6: note select, which frequency bit the key code takes
	RegOpFlags          = 32  // tremolo (bit 7), vibrato (6), sustain (5), key-scale rate (4), multiple
	RegOpLevel          = 64  // key-scale level (bits 6-7), total level
	RegOpAttackDecay    = 96  // attack rate (high nibble), decay rate
	RegOpSustainRelease = 128 // sustain level (high nibble), release rate
	RegFreqLow          = 160 // the low 8 bits of the frequency number
	RegFreqHigh         = 176 // key on (bit 5), block (bits 2-4), the frequency number's top bits
	RegRhythm           = 189 // deep tremolo (bit 7), deep vibrato (6), rhythm mode (5), drum keys
	RegFeedback         = 192 // feedback (bits 1-3), additive connection (bit 0)
	RegWaveform         = 224 // waveform 0-3
	OpGroup             = 32
)

// The rhythm register's drum keys.
const (
	DrumHiHat  = 1 << 0
	DrumCymbal = 1 << 1
	DrumTom    = 1 << 2
	DrumSnare  = 1 << 3
	DrumBass   = 1 << 4
	RhythmMode = 1 << 5
)

const (
	slots          = 18
	maxAttenuation = 511 // an envelope fully released, in 0.1875 dB steps
	sineSteps      = 256 // a quarter sine wave
	silentLevel    = 4096
	maxLogLevel    = 8191
	tremoloSteps   = 210 // the tremolo counter's period; it rises to half of it and falls
	noiseBits      = 23

	// the slots of the rhythm mode instruments
	bassModulatorSlot = 12
	bassCarrierSlot   = 15
	hiHatSlot         = 13
	tomSlot           = 14
	snareSlot         = 16
	cymbalSlot        = 17
	drumFirst         = 6 // the channels from here on are the drums in rhythm mode
	bassDrum          = 6
	hiHatSnare        = 7
	tomCymbal         = 8
)

// The chip's tables: the log-sine and exponent tables are derived from their defining
// formulas (they match the chip's ROM dumps at their ends).
var (
	logSine, exponent [sineSteps]uint16

	// twice each multiple setting's frequency multiple
	doubleMultiple = [16]uint32{1, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 20, 24, 24, 30, 30}
	keyScaleLevel  = [16]int{0, 32, 40, 45, 48, 51, 53, 55, 56, 58, 59, 60, 61, 62, 63, 64}
	keyScaleShift  = [4]uint{8, 1, 2, 0}
	// the extra envelope steps of the fast rates, by the rate's low bits and the timer's
	envelopeStep = [4][4]uint8{{0, 0, 0, 0}, {1, 0, 0, 0}, {1, 0, 1, 0}, {1, 1, 1, 0}}
)

func init() {
	for i := range sineSteps {
		logSine[i] = uint16(math.Round(-math.Log2(math.Sin((float64(i)+0.5)*math.Pi/(2*sineSteps))) * sineSteps))
		exponent[i] = uint16(math.Round(math.Exp2(float64(sineSteps-1-i)/sineSteps) * 1024))
	}
}

// Envelope stages.
const (
	attack = iota
	decay
	sustain
	release
)

// Who keys an operator on: its channel's key bit or, in rhythm mode, its drum's.
const (
	keyedByChannel = 1 << 0
	keyedByDrum    = 1 << 1
)

type operator struct {
	ch                                        *channel
	tremolo, vibrato, sustained, keyScaleRate bool
	multiple, keyScaleLevel, totalLevel       uint8
	attack, decay, sustainLevel, releaseRate  uint8
	waveform                                  uint8
	keys                                      uint8
	stage                                     int
	envelope, output, keyScaleAttenuation     uint16 // envelope: the generator's level; output: with the level, key scaling and tremolo
	phase                                     uint32
	phaseOut                                  uint16
	phaseReset                                bool
	out, previousOut, feedback                int16
	modulation                                *int16
}

type channel struct {
	fnum               uint16
	block, keyCode     uint8
	feedback           uint8
	additive           bool
	modulator, carrier *operator
}

// Chip is one OPL2. The zero value is not usable; use New.
type Chip struct {
	op   [slots]operator
	ch   [Channels]channel
	regs [256]byte

	waveSelect, noteSelect     bool
	rhythm                     uint8
	tremoloShift, vibratoShift uint

	timer                      uint32
	envTimer                   uint64
	envTimerLow, envAdd        uint8
	envOddSample, envTimerWrap bool
	tremoloPos, tremoloLevel   uint8
	vibratoPos                 uint8

	noise                                      uint32
	hiHatBit2, hiHatBit3, hiHatBit7, hiHatBit8 uint16
	cymbalBit3, cymbalBit5                     uint16
	zero                                       int16

	pending [][2]byte
	next    int
}

// New returns a chip in its reset state.
func New() *Chip {
	c := &Chip{tremoloShift: 4, vibratoShift: 1, noise: 1}
	for s := range c.op {
		o := &c.op[s]
		ch := &c.ch[s%3+s/6*3]
		o.ch = ch
		if s/3%2 == 0 {
			ch.modulator = o
		} else {
			ch.carrier = o
		}
		o.envelope, o.output, o.stage = maxAttenuation, maxAttenuation, release
	}
	for i := range c.ch {
		c.setupConnection(i)
	}
	return c
}

// Write writes val to register reg. The chip takes one write a sample, in order: an
// AdLib needs some 23 microseconds after each write, longer than a sample, so a key off
// and the key on after it reach the chip in different samples and the note restarts.
func (c *Chip) Write(reg int, val byte) {
	reg &= 255
	c.regs[reg] = val
	c.pending = append(c.pending, [2]byte{byte(reg), val})
}

// Reg is the last value written to register reg.
func (c *Chip) Reg(reg int) byte { return c.regs[reg&255] }

// Silent reports whether channel ch's carrier has died away: its envelope is fully
// attenuated.
func (c *Chip) Silent(ch int) bool { return c.ch[ch].carrier.envelope >= maxAttenuation }

// slotOf is the slot of an operator register's offset within its group, or -1.
func slotOf(offset int) int {
	if offset >= 22 || offset%8 >= 6 {
		return -1
	}
	return offset/8*6 + offset%8
}

func (c *Chip) apply(reg int, v byte) {
	switch {
	case reg == RegTest:
		c.waveSelect = v&(1<<5) != 0
	case reg == RegKeyboardSplit:
		c.noteSelect = v&(1<<6) != 0
	case reg == RegRhythm:
		c.tremoloShift = 4
		if v&(1<<7) != 0 {
			c.tremoloShift = 2
		}
		c.vibratoShift = 1
		if v&(1<<6) != 0 {
			c.vibratoShift = 0
		}
		c.rhythm = v % 64
		c.updateRhythm()
	case reg >= RegOpFlags && reg < RegFreqLow || reg >= RegWaveform && reg < RegWaveform+22:
		s := slotOf(reg % OpGroup)
		if s < 0 {
			return
		}
		o := &c.op[s]
		switch reg / OpGroup * OpGroup {
		case RegOpFlags:
			o.tremolo, o.vibrato = v&(1<<7) != 0, v&(1<<6) != 0
			o.sustained, o.keyScaleRate = v&(1<<5) != 0, v&(1<<4) != 0
			o.multiple = v % 16
		case RegOpLevel:
			o.keyScaleLevel, o.totalLevel = v/64, v%64
			o.updateKeyScale()
		case RegOpAttackDecay:
			o.attack, o.decay = v/16, v%16
		case RegOpSustainRelease:
			o.sustainLevel, o.releaseRate = v/16, v%16
			if o.sustainLevel == 15 {
				o.sustainLevel = 31 // the bottom: the whole range of the envelope
			}
		case RegWaveform:
			o.waveform = v % 4
		}
	case reg >= RegFreqLow && reg < RegFreqLow+Channels:
		ch := &c.ch[reg-RegFreqLow]
		ch.fnum = ch.fnum/256*256 + uint16(v)
		c.updateFrequency(ch)
	case reg >= RegFreqHigh && reg < RegFreqHigh+Channels:
		ch := &c.ch[reg-RegFreqHigh]
		ch.fnum = ch.fnum%256 + uint16(v%4)*256
		ch.block = v / 4 % 8
		c.updateFrequency(ch)
		key := v&(1<<5) != 0
		ch.modulator.key(keyedByChannel, key)
		ch.carrier.key(keyedByChannel, key)
	case reg >= RegFeedback && reg < RegFeedback+Channels:
		i := reg - RegFeedback
		c.ch[i].feedback, c.ch[i].additive = v/2%8, v%2 != 0
		c.setupConnection(i)
	}
}

func (o *operator) key(by uint8, on bool) {
	if on {
		o.keys |= by
	} else {
		o.keys &^= by
	}
}

func (o *operator) updateKeyScale() {
	ksl := keyScaleLevel[o.ch.fnum/64]*4 - (8-int(o.ch.block))*32
	o.keyScaleAttenuation = uint16(max(ksl, 0))
}

func (c *Chip) updateFrequency(ch *channel) {
	bit := uint16(9)
	if c.noteSelect {
		bit = 8
	}
	ch.keyCode = ch.block*2 + uint8(ch.fnum>>bit%2)
	ch.modulator.updateKeyScale()
	ch.carrier.updateKeyScale()
}

// setupConnection points channel i's operators at what modulates them: the modulator's
// own feedback, and the modulator for the carrier unless the connection is additive. In
// rhythm mode the hi-hat, snare, tom and cymbal are unmodulated single operators.
func (c *Chip) setupConnection(i int) {
	ch := &c.ch[i]
	ch.modulator.modulation = &ch.modulator.feedback
	ch.carrier.modulation = &ch.modulator.out
	if ch.additive {
		ch.carrier.modulation = &c.zero
	}
	if c.rhythm&RhythmMode != 0 && i > bassDrum {
		ch.modulator.modulation, ch.carrier.modulation = &c.zero, &c.zero
	}
}

// updateRhythm keys the drums as the rhythm register says; out of rhythm mode no
// operator is keyed as a drum.
func (c *Chip) updateRhythm() {
	for i := drumFirst; i < Channels; i++ {
		c.setupConnection(i)
	}
	on := c.rhythm&RhythmMode != 0
	drum := func(bit uint8, slots ...int) {
		for _, s := range slots {
			c.op[s].key(keyedByDrum, on && c.rhythm&bit != 0)
		}
	}
	drum(DrumHiHat, hiHatSlot)
	drum(DrumCymbal, cymbalSlot)
	drum(DrumTom, tomSlot)
	drum(DrumSnare, snareSlot)
	drum(DrumBass, bassModulatorSlot, bassCarrierSlot)
}

// envelopeStep runs operator o's envelope generator one sample.
func (c *Chip) envelopeStep(o *operator) {
	tremolo := uint16(0)
	if o.tremolo {
		tremolo = uint16(c.tremoloLevel)
	}
	o.output = min(o.envelope+uint16(o.totalLevel)*4+o.keyScaleAttenuation>>keyScaleShift[o.keyScaleLevel]+tremolo, maxAttenuation)

	keyed := o.keys != 0
	reset := false
	var rate uint8
	switch {
	case keyed && o.stage == release:
		reset, rate = true, o.attack
	case o.stage == attack:
		rate = o.attack
	case o.stage == decay:
		rate = o.decay
	case o.stage == sustain:
		if !o.sustained {
			rate = o.releaseRate
		}
	default:
		rate = o.releaseRate
	}
	o.phaseReset = reset

	keyScale := o.ch.keyCode
	if !o.keyScaleRate {
		keyScale /= 4
	}
	effective := keyScale + rate*4
	high, low := effective/4, effective%4
	if high >= 16 {
		high = 15
	}
	shift := uint8(0)
	if rate != 0 {
		if high < 12 {
			if c.envOddSample {
				switch high + c.envAdd {
				case 12:
					shift = 1
				case 13:
					shift = low / 2 % 2
				case 14:
					shift = low % 2
				}
			}
		} else {
			shift = high%4 + envelopeStep[low][c.envTimerLow]
			if shift >= 4 {
				shift = 3
			}
			if shift == 0 && c.envOddSample {
				shift = 1
			}
		}
	}

	level := o.envelope
	off := o.envelope >= maxAttenuation/8*8 // the top 6 bits all set: no sound left
	if reset && high == 15 {
		level = 0
	}
	if o.stage != attack && !reset && off {
		level = maxAttenuation
	}
	var inc uint16
	switch o.stage {
	case attack:
		if o.envelope == 0 {
			o.stage = decay
		} else if keyed && shift > 0 && high != 15 {
			inc = uint16(int16(^o.envelope) >> (4 - shift)) // a step towards 0, shrinking as it nears
		}
	case decay:
		if o.envelope/16 == uint16(o.sustainLevel) {
			o.stage = sustain
		} else if !off && !reset && shift > 0 {
			inc = 1 << (shift - 1)
		}
	default:
		if !off && !reset && shift > 0 {
			inc = 1 << (shift - 1)
		}
	}
	o.envelope = (level + inc) % (maxAttenuation + 1)
	if reset {
		o.stage = attack
	}
	if !keyed {
		o.stage = release
	}
}

// phaseStep runs slot s's phase generator one sample and, in rhythm mode, makes the
// hi-hat's, snare's and cymbal's phase of the hi-hat's and cymbal's phase bits and the
// noise. The noise generator steps once a slot.
func (c *Chip) phaseStep(s int) {
	o := &c.op[s]
	f := o.ch.fnum
	if o.vibrato {
		r := f / 128 % 8
		switch {
		case c.vibratoPos%4 == 0:
			r = 0
		case c.vibratoPos%2 != 0:
			r /= 2
		}
		r >>= c.vibratoShift
		if c.vibratoPos >= 4 {
			f -= r
		} else {
			f += r
		}
	}
	phase := uint16(o.phase >> 9)
	if o.phaseReset {
		o.phase = 0
	}
	o.phase += (uint32(f) << o.ch.block >> 1) * doubleMultiple[o.multiple] / 2
	o.phaseOut = phase

	rhythm := c.rhythm&RhythmMode != 0
	if s == hiHatSlot {
		c.hiHatBit2, c.hiHatBit3 = phase>>2%2, phase>>3%2
		c.hiHatBit7, c.hiHatBit8 = phase>>7%2, phase>>8%2
	}
	if s == cymbalSlot && rhythm {
		c.cymbalBit3, c.cymbalBit5 = phase>>3%2, phase>>5%2
	}
	if rhythm {
		ring := (c.hiHatBit2 ^ c.hiHatBit7) | (c.hiHatBit3 ^ c.cymbalBit5) | (c.cymbalBit3 ^ c.cymbalBit5)
		noise := uint16(c.noise % 2)
		switch s {
		case hiHatSlot:
			o.phaseOut = ring << 9
			if ring^noise != 0 {
				o.phaseOut |= 208
			} else {
				o.phaseOut |= 52
			}
		case snareSlot:
			o.phaseOut = c.hiHatBit8<<9 | (c.hiHatBit8^noise)<<8
		case cymbalSlot:
			o.phaseOut = ring<<9 | 128
		}
	}
	feedback := (c.noise>>14 ^ c.noise) % 2
	c.noise = c.noise>>1 | feedback<<(noiseBits-1)
}

func exponential(level uint32) int16 {
	level = min(level, maxLogLevel)
	return int16(uint32(exponent[level%sineSteps]) * 2 >> (level / sineSteps))
}

// generate is operator o's output for its phase, modulation, waveform and envelope.
func (c *Chip) generate(o *operator) {
	phase := (o.phaseOut + uint16(*o.modulation)) % 1024
	quarter := phase % sineSteps
	if phase&sineSteps != 0 {
		quarter = sineSteps - 1 - quarter // the falling quarters mirror the rising ones
	}
	secondHalf := phase&(2*sineSteps) != 0
	waveform := o.waveform
	if !c.waveSelect {
		waveform = 0
	}
	level := uint32(logSine[quarter])
	negative := false
	switch waveform {
	case 0: // sine
		negative = secondHalf
	case 1: // half sine
		if secondHalf {
			level = silentLevel
		}
	case 3: // the rising quarters only
		if phase&sineSteps != 0 {
			level = silentLevel
		}
	}
	out := exponential(level + uint32(o.output)*8)
	if negative {
		out = ^out // the chip negates in one's complement
	}
	o.out = out
}

// step applies the next pending write and runs the chip one sample.
func (c *Chip) step() {
	if c.next < len(c.pending) {
		w := c.pending[c.next]
		c.next++
		if c.next == len(c.pending) {
			c.pending, c.next = c.pending[:0], 0
		}
		c.apply(int(w[0]), w[1])
	}
	for s := range c.op {
		o := &c.op[s]
		if fb := o.ch.feedback; fb != 0 {
			o.feedback = (o.previousOut + o.out) >> (9 - fb)
		} else {
			o.feedback = 0
		}
		o.previousOut = o.out
		c.envelopeStep(o)
		c.phaseStep(s)
		c.generate(o)
	}

	if c.timer%64 == 63 {
		c.tremoloPos = (c.tremoloPos + 1) % tremoloSteps
	}
	if c.tremoloPos < tremoloSteps/2 {
		c.tremoloLevel = c.tremoloPos >> c.tremoloShift
	} else {
		c.tremoloLevel = (tremoloSteps - c.tremoloPos) >> c.tremoloShift
	}
	if c.timer%1024 == 1023 {
		c.vibratoPos = (c.vibratoPos + 1) % 8
	}
	c.timer++

	// the envelope timer: on odd samples, the number of its trailing zero bits picks
	// which slow rates step
	if c.envOddSample {
		zeros := uint8(0)
		for zeros < 36 && c.envTimer>>zeros%2 == 0 {
			zeros++
		}
		c.envAdd = 0
		if zeros <= 12 {
			c.envAdd = zeros + 1
		}
		c.envTimerLow = uint8(c.envTimer % 4)
	}
	if c.envTimerWrap || c.envOddSample {
		if c.envTimer == 1<<36-1 {
			c.envTimer, c.envTimerWrap = 0, true
		} else {
			c.envTimer, c.envTimerWrap = c.envTimer+1, false
		}
	}
	c.envOddSample = !c.envOddSample
}

// channelOutput is channel i's output after a step: in rhythm mode the bass drum's
// carrier and the other drums sound twice as loud as an operator.
func (c *Chip) channelOutput(i int) int {
	ch := &c.ch[i]
	if c.rhythm&RhythmMode != 0 && i >= drumFirst {
		if i == bassDrum {
			return 2 * int(ch.carrier.out)
		}
		return 2 * (int(ch.modulator.out) + int(ch.carrier.out))
	}
	out := int(ch.carrier.out)
	if ch.additive {
		out += int(ch.modulator.out)
	}
	return out
}

// Sample runs the chip one sample and returns its output, -32768 to 32767.
func (c *Chip) Sample() int {
	c.step()
	sum := 0
	for i := range c.ch {
		sum += c.channelOutput(i)
	}
	return max(-32768, min(32767, sum))
}

// SampleChannels runs the chip one sample and stores each channel's output in out. In
// rhythm mode channel 6 is the bass drum, 7 the hi-hat and snare, 8 the tom and cymbal.
func (c *Chip) SampleChannels(out *[Channels]int) {
	c.step()
	for i := range c.ch {
		out[i] = c.channelOutput(i)
	}
}

// Resampler plays a chip at another sample rate, interpolating between its samples.
type Resampler struct {
	Chip *Chip
	// Mix, if set, makes each of the chip's samples from its channels' outputs (to weigh
	// them, say) in place of Sample's plain sum.
	Mix        func(ch *[Channels]int) int
	rate, frac int
	ch         [Channels]int
	prev, cur  int
}

// NewResampler returns a resampler of chip to rate Hz.
func NewResampler(chip *Chip, rate int) *Resampler {
	return &Resampler{Chip: chip, rate: rate}
}

// Next is the next sample at the resampler's rate.
func (r *Resampler) Next() int {
	for r.frac >= r.rate {
		r.frac -= r.rate
		r.prev = r.cur
		if r.Mix != nil {
			r.Chip.SampleChannels(&r.ch)
			r.cur = r.Mix(&r.ch)
		} else {
			r.cur = r.Chip.Sample()
		}
	}
	v := (r.prev*(r.rate-r.frac) + r.cur*r.frac) / r.rate
	r.frac += Rate
	return v
}

// Generate fills out with samples at the resampler's rate.
func (r *Resampler) Generate(out []int16) {
	for i := range out {
		out[i] = int16(r.Next())
	}
}
