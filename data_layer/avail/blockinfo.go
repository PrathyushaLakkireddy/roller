package avail

import (
	"fmt"
	"log"
	"strings"

	"github.com/availproject/avail-go-sdk/src/rpc"
	"github.com/dymensionxyz/roller/utils/roller"
)

func GetAvailBlockLatest(raCfg roller.RollappConfig) (string, string, error) {
	api, err := NewSDK(raCfg.DA.ApiUrl)
	if err != nil {
		fmt.Printf("cannot create api:%v", err)
	}
	if api == nil || api.Client == nil {
		log.Fatal("API client is not properly initialized")
	}
	resp, err := rpc.GetAvailBlockLatest(api.Client)
	if err != nil {
		fmt.Printf("cannot author rotate:%v", err)
	}
	fmt.Println(resp)
	blockNumber := string(resp.Block.Header.Number)
	// parentHash := string(resp.Block.Header.ParentHash)

	// get block hash
	blockHash, err := rpc.GetBlockHash(api.Client, uint64(resp.Block.Header.Number))
	if err != nil {
		fmt.Println("cannot get blockchash")
	}

	return blockNumber, blockHash.Hex(), err
}

func GetBlockByHeight() {
	// api, err := NewSDK(raCfg.DA.ApiUrl)
	// if err != nil {
	// 	fmt.Printf("cannot create api:%v", err)
	// }
	// if api == nil || api.Client == nil {
	// 	log.Fatal("API client is not properly initialized")
	// }

	// rpc.GetAvailBlock()

}

// ExtractHeightfromDAPath function extracts the celestia height from DA path that's
// available on the hub
func ExtractHeightfromDAPath(input string) (string, error) {
	parts := strings.Split(input, "|")
	if len(parts) < 2 {
		return "", fmt.Errorf("input string does not have enough parts")
	}
	return parts[1], nil
}
