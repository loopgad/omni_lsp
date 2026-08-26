package main

import "fmt"

// Greet returns a localized greeting. The exclamation mark is ASCII; the
// name may contain multi-byte runes (UTF-16 position coverage).
func Greet(name string) string {
	return fmt.Sprintf("你好, %s!", name)
}

// Add is a trivial generic helper (generics exercise type checking paths).
func Add[T int | float64](a, b T) T {
	return a + b
}

type point struct{ x, y int }
