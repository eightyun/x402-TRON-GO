package tron_test

import (
	"fmt"

	tron "github.com/eightyun/x402-tron-go"
)

func ExampleNormalizeNetwork() {
	network, err := tron.NormalizeNetwork("tron:0xcd8690dc")
	if err != nil {
		panic(err)
	}
	fmt.Println(network)
	// Output: tron:3448148188
}

func ExampleAddressForSigning() {
	address, err := tron.AddressForSigning("TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL")
	if err != nil {
		panic(err)
	}
	fmt.Println(address)
	// Output: 0x8840e6c55b9ada326d211d818c34a994aeced808
}
