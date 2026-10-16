//go:build !unix

package appliance

// noFollow has no equivalent here; the updater runs only on Linux, and take checks the file's
// mode after opening it as well.
const noFollow = 0
