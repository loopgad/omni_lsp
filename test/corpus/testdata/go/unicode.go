package corpus

// Unicode identifiers and emoji comments stress position mapping: every
// rune after the BMP costs two UTF-16 code units.
const pi = 3.14159 // π≈3.14, 🌍 round

func Σ(values ...float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func useΣ() { _ = Σ(1, 2) }
