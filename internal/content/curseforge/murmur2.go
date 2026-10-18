package curseforge

// murmur2 implements MurmurHash2 (32-bit, little-endian) with a configurable
// seed, as used by CurseForge for file fingerprinting.
//
// The algorithm: https://sites.google.com/site/murmurhash/
// CurseForge uses seed=1 and strips whitespace bytes (9, 10, 13, 32) from the
// input before hashing when computing manifest fingerprints.

const murmur2Seed uint32 = 1

const (
	murmur2M uint32 = 0x5bd1e995
	murmur2R uint32 = 24
)

// murmur2 computes MurmurHash2 over the given data with seed 1.
func murmur2(data []byte) uint32 {
	return murmur2WithSeed(data, murmur2Seed)
}

// murmur2WithSeed computes MurmurHash2 over the given data with the given seed.
func murmur2WithSeed(data []byte, seed uint32) uint32 {
	h := seed ^ uint32(len(data))
	n := len(data) / 4

	for i := 0; i < n; i++ {
		off := i * 4
		k := uint32(data[off]) |
			uint32(data[off+1])<<8 |
			uint32(data[off+2])<<16 |
			uint32(data[off+3])<<24

		k *= murmur2M
		k ^= k >> murmur2R
		k *= murmur2M

		h *= murmur2M
		h ^= k
	}

	rem := data[n*4:]
	switch len(rem) {
	case 3:
		h ^= uint32(rem[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(rem[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(rem[0])
		h *= murmur2M
	}

	h ^= h >> 13
	h *= murmur2M
	h ^= h >> 15
	return h
}

// murmur2StripWhitespace computes MurmurHash2 after removing whitespace bytes
// (0x09 tab, 0x0a newline, 0x0d carriage-return, 0x20 space) from the input.
// CurseForge uses this form for modpack manifest fingerprints.
func murmur2StripWhitespace(data []byte) uint32 {
	stripped := make([]byte, 0, len(data))
	for _, b := range data {
		if b != 0x09 && b != 0x0a && b != 0x0d && b != 0x20 {
			stripped = append(stripped, b)
		}
	}
	return murmur2WithSeed(stripped, murmur2Seed)
}
