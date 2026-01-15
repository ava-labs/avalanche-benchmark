package bombard

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
)

// ToWei converts a float amount to wei (18 decimals).
func ToWei(amount float64) *big.Int {
	const decimals = 18

	wei := new(big.Int)

	// Convert float to string with maximum precision
	amountStr := fmt.Sprintf("%.18f", amount)

	// Remove decimal point and trailing zeros
	amountStr = strings.Replace(amountStr, ".", "", 1)
	amountStr = strings.TrimRight(amountStr, "0")

	// Parse string to big.Int
	wei.SetString(amountStr, 10)

	// Adjust for decimals
	multiplier := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	wei.Mul(wei, multiplier)

	return wei
}

// ERC20TransferSelector is the function selector for transfer(address,uint256)
// keccak256("transfer(address,uint256)")[:4] = 0xa9059cbb
var ERC20TransferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

// EncodeERC20Transfer encodes calldata for ERC20 transfer(address,uint256)
func EncodeERC20Transfer(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 68) // 4 (selector) + 32 (address) + 32 (amount)

	// Function selector
	copy(data[0:4], ERC20TransferSelector)

	// Pad address to 32 bytes (left-padded with zeros)
	copy(data[16:36], to.Bytes())

	// Amount as 32 bytes (left-padded with zeros)
	amountBytes := amount.Bytes()
	copy(data[68-len(amountBytes):68], amountBytes)

	return data
}

// PredeployedTokenAddr is the address of the predeployed ERC20 Benchmark Token
var PredeployedTokenAddr = common.HexToAddress(PredeployedTokenAddress)
