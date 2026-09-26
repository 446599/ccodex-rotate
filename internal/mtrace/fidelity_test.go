package mtrace

import (
	"fmt"
	"testing"
)

func TestFidelityVsPython(t *testing.T) {
	b, err := LoadBank()
	if err != nil {
		t.Fatal(err)
	}
	nums := make([]int, 300)
	for i := range nums {
		nums[i] = (i * 37 % 355) + 1
	}
	fused := robustScoreNumbers(nums, b)
	fmt.Printf("fused[:6]:")
	for _, v := range fused[:6] {
		fmt.Printf(" %.9f", v)
	}
	fmt.Printf("\n")
	fmt.Printf("fused[-3:]:")
	for _, v := range fused[len(fused)-3:] {
		fmt.Printf(" %.9f", v)
	}
	fmt.Printf("\n")
}
