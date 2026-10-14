# opl2

A Yamaha YM3812 (OPL2, the AdLib's chip) emulator in plain Go, with no cgo and
no dependencies. It is integer-exact in the manner of Nuked OPL3: the chip's own
log-sine and exponent tables, envelope generator, phase generator, tremolo and
vibrato, the 23-bit noise generator and rhythm mode (bass drum, snare, tom,
cymbal, hi-hat).

It is the OPL2 of the goplay ports (Eye of the Beholder, Ultima Underworld,
Ultima VI, Lands of Lore, Might and Magic III). Each of them had its own core;
this module merges them into one. Without rhythm mode it matches Eye of the
Beholder's core bit for bit (checked over two million samples of random
register writes). Rhythm mode and the per-sample register queue are new.

```go
chip := opl2.New()
chip.Write(opl2.RegFreqLow, 68)            // queued: one write per sample, as the bus allows
chip.Write(opl2.RegFreqHigh, 1<<5|4<<2|2)  // key on, block 4
v := chip.Sample()                         // one sample at opl2.Rate (49716 Hz)

out := opl2.NewResampler(chip, 44100)      // at the output's rate, interpolated
out.Mix = func(ch *[opl2.Channels]int) int { // optional: weigh the channels
	return ch[0] + ch[1]/2
}
pcm := make([]int16, 1024)
out.Generate(pcm)
```

- `Write(reg, val)` queues a register write; the chip applies one per
  sample, so a key-off and a key-on in a row retrigger the note as on the
  real chip. `Reg(reg)` reads back the last value written.
- `Sample()` is the mixed output, clamped to 16 bits; `SampleChannels` gives
  each channel's output, for panning or weighing them yourself.
- `Silent(ch)` tells whether a channel's operators have died away.
- The register constants (`RegOpLevel`, `RegFreqHigh`, `RegRhythm`, ...) and
  the drum bits (`DrumBass` ... `DrumHiHat`, `RhythmMode`) name the chip's
  registers.

A silent operator outputs -1 rather than 0, the chip's one's-complement
negation, so a quiet chip has a tiny DC offset.

## Versions

v0.1.0 is the core as it came out of the ports. The working tree adds
`Resampler.Mix`, which Might and Magic III uses to put its music and its
effects on separate volume faders; until that is tagged, mm3-go builds
against this directory through `replace github.com/memmaker/opl2 => ../opl2`.
