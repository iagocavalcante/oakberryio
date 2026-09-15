package daemon

import "fmt"

// bridgeName returns tenant subnet idx's bridge name, e.g. idx 3 -> "oak3".
func bridgeName(idx int) string {
	return fmt.Sprintf("oak%d", idx)
}

// gatewayIP returns tenant subnet idx's gateway address, e.g. idx 3 ->
// "10.200.3.1".
func gatewayIP(idx int) string {
	return fmt.Sprintf("10.200.%d.1", idx)
}
