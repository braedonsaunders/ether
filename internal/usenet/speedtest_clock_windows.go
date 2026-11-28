package usenet

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var queryUnbiasedInterruptTimePrecise = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTimePrecise")

func speedTransferTime() time.Duration {
	// A buffered BODY can start and end on the same Go timer tick.
	// The precise interrupt clock is monotonic and reports 100 ns units.
	var ticks uint64
	queryUnbiasedInterruptTimePrecise.Call(uintptr(unsafe.Pointer(&ticks)))
	return time.Duration(ticks) * 100 * time.Nanosecond
}
