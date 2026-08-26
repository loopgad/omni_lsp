package query

// maxRecursionDepth bounds the RecursionAllowed escape hatch so a runaway
// language-semantic cycle terminates deterministically.
const maxRecursionDepth = 32
