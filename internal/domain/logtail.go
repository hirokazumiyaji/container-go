package domain

// LastNBytes returns up to n trailing bytes from data.
func LastNBytes(data []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	if len(data) <= n {
		return data
	}
	return data[len(data)-n:]
}
