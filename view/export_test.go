package view

import "archive/zip"

// DeflateMethod is [zip.Deflate], exposed so the external test package can drive
// [DecompressMemberBoundedForTest] without importing archive/zip.
const DeflateMethod = zip.Deflate

// DecompressMemberBoundedForTest exposes [decompressMemberBounded] to tests, so
// the decompression cap can be exercised with a small limit rather than a
// gibibyte-scale fixture.
func DecompressMemberBoundedForTest(method uint16, compressed []byte, limit int64) ([]byte, error) {
	return decompressMemberBounded(method, compressed, limit)
}
