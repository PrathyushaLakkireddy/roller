package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dymensionxyz/roller/utils/config"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	initconfig "github.com/dymensionxyz/roller/cmd/config/init"
	"github.com/dymensionxyz/roller/cmd/consts"
	"github.com/dymensionxyz/roller/relayer"
	"github.com/dymensionxyz/roller/sequencer"
	"github.com/dymensionxyz/roller/utils/config/tomlconfig"
	"github.com/dymensionxyz/roller/utils/config/yamlconfig"
	dymintutils "github.com/dymensionxyz/roller/utils/dymint"
	"github.com/dymensionxyz/roller/utils/errorhandling"
	"github.com/dymensionxyz/roller/utils/filesystem"
	genesisutils "github.com/dymensionxyz/roller/utils/genesis"
	"github.com/dymensionxyz/roller/utils/keys"
	"github.com/dymensionxyz/roller/utils/logging"
	"github.com/dymensionxyz/roller/utils/rollapp"
	rollapputils "github.com/dymensionxyz/roller/utils/rollapp"
	"github.com/dymensionxyz/roller/utils/roller"
	sequencerutils "github.com/dymensionxyz/roller/utils/sequencer"
)

// TODO: Test relaying on 35-C and update the prices
const (
	flagOverride = "override"
)

// TODO: cleanup required, a lot of duplicate code in this cmd
func Cmd() *cobra.Command {
	relayerStartCmd := &cobra.Command{
		Use:   "setup",
		Short: "Setup IBC connection between the Dymension hub and the RollApp.",
		Run: func(cmd *cobra.Command, args []string) {
			// TODO: there are too many things set here, might be worth to refactor
			home, _ := filesystem.ExpandHomePath(
				cmd.Flag(initconfig.GlobalFlagNames.Home).Value.String(),
			)
			relayerHome := filepath.Join(home, consts.ConfigDirName.Relayer)

			// check for roller config, if it's present - fetch the rollapp ID from there
			var raID string
			var env string
			var hd consts.HubData
			var runForExisting bool
			var rollerData roller.RollappConfig
			var existingRollerConfig bool

			rollerConfigFilePath := filepath.Join(home, consts.RollerConfigFileName)

			// fetch rollapp metadata from chain
			// retrieve rpc endpoint
			_, err := os.Stat(rollerConfigFilePath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					pterm.Info.Println("existing roller configuration not found")
					existingRollerConfig = false
					runForExisting = false
				} else {
					pterm.Error.Println("failed to check existing roller config")
					return
				}
			} else {
				pterm.Info.Println("existing roller configuration found, retrieving RollApp ID from it")
				rollerData, err = roller.LoadConfig(home)
				if err != nil {
					pterm.Error.Printf("failed to load rollapp config: %v\n", err)
					return
				}
				rollerRaID := rollerData.RollappID
				rollerHubData := rollerData.HubData
				msg := fmt.Sprintf(
					"the retrieved rollapp ID is: %s, would you like to initialize the relayer for this rollapp?",
					rollerRaID,
				)
				rlyFromRoller, _ := pterm.DefaultInteractiveConfirm.WithDefaultText(msg).Show()
				if rlyFromRoller {
					raID = rollerRaID
					hd = rollerHubData
					runForExisting = true
				}

				if !rlyFromRoller {
					runForExisting = false
				}
			}

			if !runForExisting {
				raID, _ = pterm.DefaultInteractiveTextInput.WithDefaultText("Please enter the RollApp ID").
					Show()
			}

			_, err = rollapputils.ValidateChainID(raID)
			if err != nil {
				pterm.Error.Printf("'%s' is not a valid RollApp ID: %v", raID, err)
				return
			}

			if !runForExisting {
				envs := []string{"playground", "custom"}
				env, _ = pterm.DefaultInteractiveSelect.
					WithDefaultText(
						"select the environment you want to initialize relayer for",
					).
					WithOptions(envs).
					Show()

				if env == "playground" {
					hd = consts.Hubs[env]
				} else {
					hd = config.GenerateCustomHubData()
					// err = dependencies.InstallCustomDymdVersion()
					// if err != nil {
					// 	pterm.Error.Println("failed to install custom dymd version: ", err)
					// 	return
					// }
				}

				if !existingRollerConfig {
					pterm.Info.Println("creating a new roller config")
					err := os.MkdirAll(home, 0o755)
					if err != nil {
						pterm.Error.Printf(
							"failed to create %s: %v", home, err,
						)
						return
					}

					_, err = os.Create(rollerConfigFilePath)
					if err != nil {
						pterm.Error.Printf(
							"failed to create %s: %v", rollerConfigFilePath, err,
						)
						return
					}
				}

				rollerTomlData := map[string]any{
					"rollapp_id": raID,
					"home":       home,

					"HubData.id":              hd.ID,
					"HubData.api_url":         hd.API_URL,
					"HubData.rpc_url":         hd.RPC_URL,
					"HubData.archive_rpc_url": hd.ARCHIVE_RPC_URL,
					"HubData.gas_price":       hd.GAS_PRICE,
				}

				for key, value := range rollerTomlData {
					err = tomlconfig.UpdateFieldInFile(
						rollerConfigFilePath,
						key,
						value,
					)
					if err != nil {
						fmt.Printf("failed to add %s to roller.toml: %v", key, err)
						return
					}
				}
			}

			// retrieve rollapp rpc endpoints
			raRpc, err := sequencerutils.GetRpcEndpointFromChain(raID, hd)
			if err != nil {
				return
			}

			// check if there are active channels created for the rollapp
			relayerLogFilePath := logging.GetRelayerLogPath(home)
			relayerLogger := logging.GetLogger(relayerLogFilePath)

			raData := consts.RollappData{
				ID:     raID,
				RpcUrl: fmt.Sprintf("%s:%d", raRpc, 443),
			}

			rly := relayer.NewRelayer(
				home,
				raData.ID,
				hd.ID,
			)
			rly.SetLogger(relayerLogger)
			logFileOption := logging.WithLoggerLogging(relayerLogger)

			rollappChainData, err := rollapp.GetRollappMetadataFromChain(
				home,
				raData.ID,
				&hd,
			)
			errorhandling.PrettifyErrorIfExists(err)

			isRelayerInitialized, err := filesystem.DirNotEmpty(relayerHome)
			if err != nil {
				pterm.Error.Printf("failed to check %s: %v\n", relayerHome, err)
				return
			}

			var shouldOverwrite bool
			if isRelayerInitialized {
				pterm.Info.Println("relayer already initialized")
			} else {
				err = os.MkdirAll(relayerHome, 0o755)
				if err != nil {
					pterm.Error.Printf("failed to create %s: %v\n", relayerHome, err)
					return
				}
			}

			// if shouldOverwrite {
			// 	pterm.Info.Println("overriding the existing relayer configuration")
			// 	err = os.RemoveAll(relayerHome)
			// 	if err != nil {
			// 		pterm.Error.Printf(
			// 			"failed to recuresively remove %s: %v\n",
			// 			relayerHome,
			// 			err,
			// 		)
			// 		return
			// 	}
			//
			// 	err := filesystem.RemoveServiceFiles(consts.RelayerSystemdServices)
			// 	if err != nil {
			// 		pterm.Error.Printf("failed to remove relayer systemd services: %v\n", err)
			// 		return
			// 	}
			//
			// 	err = os.MkdirAll(relayerHome, 0o755)
			// 	if err != nil {
			// 		pterm.Error.Printf("failed to create %s: %v\n", relayerHome, err)
			// 		return
			// 	}
			// }

			srcIbcChannel, dstIbcChannel, err := rly.LoadActiveChannel(raData, hd)
			if err != nil {
				pterm.Error.Printf("failed to load active channel, %v", err)
				return
			}

			if srcIbcChannel == "" || dstIbcChannel == "" {
				defer func() {
					pterm.Info.Println("reverting dymint config to 1h")
					err := dymintutils.UpdateDymintConfigForIBC(home, "1h0m0s", true)
					if err != nil {
						pterm.Error.Println("failed to update dymint config: ", err)
						return
					}
				}()

				if !runForExisting {
					pterm.Error.Println(
						"existing channels not found, initial IBC setup must be run on a sequencer node",
					)
					return
				}

				if runForExisting && rollerData.NodeType != consts.NodeType.Sequencer {
					pterm.Error.Println(
						"existing channels not found, initial IBC setup must be run on a sequencer node",
					)
					return
				}

				// at this point it is safe to assume that
				// relayer is being initialized on a sequencer node
				// there is an existing roller config that can be used as the data source

				pterm.Info.Println("let's create that IBC connection, shall we?")
				/* ---------------------------- Initialize relayer --------------------------- */
				as, err := genesisutils.GetGenesisAppState(home)
				if err != nil {
					pterm.Error.Printf("failed to get genesis app state: %v\n", err)
					return
				}
				rollappDenom := as.Bank.Supply[0].Denom

				err = tomlconfig.UpdateFieldInFile(
					rollerConfigFilePath,
					"base_denom",
					rollappDenom,
				)
				if err != nil {
					pterm.Error.Println("failed to set base denom in roller.toml")
					return
				}

				seq := sequencer.GetInstance(rollerData)

				dymintutils.WaitForHealthyRollApp("http://localhost:26657/health")
				err = relayer.WaitForValidRollappHeight(seq)
				if err != nil {
					pterm.Error.Printf("rollapp did not reach valid height: %v\n", err)
					return
				}

				if !isRelayerInitialized || shouldOverwrite {
					// preflight checks
					blockInformation, err := rollapputils.GetCurrentHeight()
					if err != nil {
						pterm.Error.Printf("failed to get current block height: %v\n", err)
						return
					}
					currentHeight, err := strconv.Atoi(
						blockInformation.Block.Header.Height,
					)
					if err != nil {
						pterm.Error.Printf("failed to get current block height: %v\n", err)
						return
					}

					if currentHeight <= 2 {
						pterm.Warning.Println("current height is too low, updating dymint config")
						err = dymintutils.UpdateDymintConfigForIBC(home, "5s", false)
						if err != nil {
							pterm.Error.Println("failed to update dymint config: ", err)
							return
						}
					}

					rollappPrefix := rollappChainData.Bech32Prefix
					if err != nil {
						pterm.Error.Printf("failed to retrieve bech32_prefix: %v\n", err)
						return
					}

					pterm.Info.Println("initializing relayer config")
					err = initconfig.InitializeRelayerConfig(
						relayer.ChainConfig{
							ID:            rollerData.RollappID,
							RPC:           consts.DefaultRollappRPC,
							Denom:         rollappDenom,
							AddressPrefix: rollappPrefix,
							GasPrices:     "2000000000",
						}, relayer.ChainConfig{
							ID:            rollerData.HubData.ID,
							RPC:           rollerData.HubData.RPC_URL,
							Denom:         consts.Denoms.Hub,
							AddressPrefix: consts.AddressPrefixes.Hub,
							GasPrices:     rollerData.HubData.GAS_PRICE,
						}, home,
					)
					if err != nil {
						pterm.Error.Printf(
							"failed to initialize relayer config: %v\n",
							err,
						)
						return
					}

					relayerKeys, err := keys.GenerateRelayerKeys(rollerData)
					if err != nil {
						pterm.Error.Printf("failed to create relayer keys: %v\n", err)
						return
					}

					for _, key := range relayerKeys {
						key.Print(keys.WithMnemonic(), keys.WithName())
					}

					keysToFund, err := keys.GetRelayerKeys(rollerData)
					pterm.Info.Println(
						"please fund the hub relayer key with at least 20 dym tokens: ",
					)
					for _, k := range keysToFund {
						k.Print(keys.WithName())
					}
					proceed, _ := pterm.DefaultInteractiveConfirm.WithDefaultValue(false).
						WithDefaultText(
							"press 'y' when the wallets are funded",
						).Show()
					if !proceed {
						return
					}

					if err != nil {
						pterm.Error.Printf("failed to create relayer keys: %v\n", err)
						return
					}

					if err := relayer.CreatePath(rollerData); err != nil {
						pterm.Error.Printf("failed to create relayer IBC path: %v\n", err)
						return
					}

					pterm.Info.Println("updating application relayer config")
					relayerConfigPath := filepath.Join(relayerHome, "config", "config.yaml")
					updates := map[string]interface{}{
						fmt.Sprintf("chains.%s.value.gas-adjustment", rollerData.HubData.ID): 1.5,
						fmt.Sprintf("chains.%s.value.gas-adjustment", rollerData.RollappID):  1.3,
						fmt.Sprintf("chains.%s.value.is-dym-hub", rollerData.HubData.ID):     true,
						fmt.Sprintf(
							"chains.%s.value.http-addr",
							rollerData.HubData.ID,
						): rollerData.HubData.API_URL,
						fmt.Sprintf("chains.%s.value.is-dym-rollapp", rollerData.RollappID): true,
						"extra-codecs": []string{
							"ethermint",
						},
					}
					err = yamlconfig.UpdateNestedYAML(relayerConfigPath, updates)
					if err != nil {
						pterm.Error.Printf("Error updating YAML: %v\n", err)
						return
					}

					err = dymintutils.UpdateDymintConfigForIBC(home, "5s", false)
					if err != nil {
						pterm.Error.Printf("Error updating YAML: %v\n", err)
						return
					}
				}

				if isRelayerInitialized && !shouldOverwrite {
					pterm.Info.Println("ensuring relayer keys are present")
					kc := keys.GetRelayerKeysConfig(rollerData)

					for k, v := range kc {
						pterm.Info.Printf("checking %s\n", k)

						switch v.ID {
						case consts.KeysIds.RollappRelayer:
							chainId := rollerData.RollappID
							isPresent, err := keys.IsRlyAddressWithNameInKeyring(v, chainId)
							if err != nil {
								pterm.Error.Printf("failed to check address: %v\n", err)
								return
							}

							if !isPresent {
								key, err := keys.AddRlyKey(v, rollerData.RollappID)
								if err != nil {
									pterm.Error.Printf("failed to add key: %v\n", err)
								}

								key.Print(keys.WithMnemonic(), keys.WithName())
							}
						case consts.KeysIds.HubRelayer:
							chainId := rollerData.HubData.ID
							isPresent, err := keys.IsRlyAddressWithNameInKeyring(v, chainId)
							if err != nil {
								pterm.Error.Printf("failed to check address: %v\n", err)
								return
							}
							if !isPresent {
								key, err := keys.AddRlyKey(v, rollerData.HubData.ID)
								if err != nil {
									pterm.Error.Printf("failed to add key: %v\n", err)
								}

								key.Print(keys.WithMnemonic(), keys.WithName())
							}
						default:
							pterm.Error.Println("invalid key name", err)
							return
						}
					}
				}

				err = VerifyRelayerBalances(raData, hd)
				if err != nil {
					return
				}

				// errorhandling.RequireMigrateIfNeeded(rollappConfig)

				err = rollerData.Validate()
				if err != nil {
					pterm.Error.Printf("failed to validate rollapp config: %v\n", err)
					return
				}

				var createIbcChannels bool

				if rly.ChannelReady() && !shouldOverwrite {
					pterm.DefaultSection.WithIndentCharacter("💈").
						Println("IBC transfer channel is already established!")

					status := fmt.Sprintf(
						"Active\nrollapp: %s\n<->\nhub: %s",
						rly.SrcChannel,
						rly.DstChannel,
					)
					err := rly.WriteRelayerStatus(status)
					if err != nil {
						fmt.Println(err)
						return
					}

					pterm.Info.Println(status)
					return
				}

				if !rly.ChannelReady() {
					createIbcChannels, _ = pterm.DefaultInteractiveConfirm.WithDefaultText(
						fmt.Sprintf(
							"no channel found. would you like to create a new IBC channel for %s?",
							rollerData.RollappID,
						),
					).Show()

					if !createIbcChannels {
						pterm.Warning.Println("you can't run a relayer without an ibc channel")
						return
					}
				}

				// TODO: look up relayer keys
				if createIbcChannels || shouldOverwrite {
					err = VerifyRelayerBalances(raData, hd)
					if err != nil {
						pterm.Error.Printf("failed to verify relayer balances: %v\n", err)
						return
					}

					pterm.Info.Println("establishing IBC transfer channel")
					seq := sequencer.GetInstance(rollerData)
					if seq == nil {
						pterm.Error.Println("failed to get sequencer sequencer instance")
						return
					}

					channels, err := rly.CreateIBCChannel(
						shouldOverwrite,
						logFileOption,
						raData,
						hd,
					)
					if err != nil {
						pterm.Error.Printf("failed to create IBC channel: %v\n", err)
						return
					}

					srcIbcChannel = channels.Src
					dstIbcChannel = channels.Dst
				}

				defer func() {
					pterm.Info.Println("next steps:")
					pterm.Info.Printf(
						"run %s load the necessary systemd services\n",
						pterm.DefaultBasicText.WithStyle(pterm.FgYellow.ToStyle()).
							Sprintf("roller relayer services load"),
					)
				}()
				return
			}

			if srcIbcChannel != "" && dstIbcChannel != "" {
				if !isRelayerInitialized || shouldOverwrite {
					raResponse, err := rollapp.Show(raID, hd)
					if err != nil {
						pterm.Error.Println("failed to retrieve rollapp information: ", err)
						return
					}

					raRpc, err := sequencerutils.GetRpcEndpointFromChain(raID, hd)
					if err != nil {
						pterm.Error.Println("failed to retrieve rollapp rpc endpoint: ", err)
						return
					}

					pterm.Info.Println("initializing relayer config")
					err = initconfig.InitializeRelayerConfig(
						relayer.ChainConfig{
							ID:            raResponse.Rollapp.RollappId,
							RPC:           raRpc,
							Denom:         raResponse.Rollapp.GenesisInfo.NativeDenom.Base,
							AddressPrefix: raResponse.Rollapp.GenesisInfo.Bech32Prefix,
							GasPrices:     "2000000000",
						}, relayer.ChainConfig{
							ID:            hd.ID,
							RPC:           hd.RPC_URL,
							Denom:         consts.Denoms.Hub,
							AddressPrefix: consts.AddressPrefixes.Hub,
							GasPrices:     hd.GAS_PRICE,
						}, home,
					)
					if err != nil {
						pterm.Error.Printf(
							"failed to initialize relayer config: %v\n",
							err,
						)
						return
					}

					relayerKeys, err := keys.GenerateRelayerKeys(rollerData)
					if err != nil {
						pterm.Error.Printf("failed to create relayer keys: %v\n", err)
						return
					}

					for _, key := range relayerKeys {
						key.Print(keys.WithMnemonic(), keys.WithName())
					}

					keysToFund, err := keys.GetRelayerKeys(rollerData)
					pterm.Info.Println(
						"please fund the hub relayer key with at least 20 dym tokens: ",
					)
					for _, k := range keysToFund {
						k.Print(keys.WithName())
					}
					proceed, _ := pterm.DefaultInteractiveConfirm.WithDefaultValue(false).
						WithDefaultText(
							"press 'y' when the wallets are funded",
						).Show()
					if !proceed {
						return
					}

					if err != nil {
						pterm.Error.Printf("failed to create relayer keys: %v\n", err)
						return
					}

					if err := relayer.CreatePath(rollerData); err != nil {
						pterm.Error.Printf("failed to create relayer IBC path: %v\n", err)
						return
					}

					pterm.Info.Println("updating application relayer config")
					relayerConfigPath := filepath.Join(relayerHome, "config", "config.yaml")

					rollappIbcConnection, hubIbcConnection, err := rly.GetActiveConnections(
						raData,
						hd,
					)
					if err != nil {
						pterm.Error.Printf("failed to retrieve active connections: %v\n", err)
						return
					}

					updates := map[string]interface{}{
						// hub
						fmt.Sprintf("chains.%s.value.gas-adjustment", rollerData.HubData.ID): 1.5,
						fmt.Sprintf("chains.%s.value.is-dym-hub", rollerData.HubData.ID):     true,
						fmt.Sprintf(
							"chains.%s.value.http-addr",
							rollerData.HubData.ID,
						): rollerData.HubData.API_URL,
						fmt.Sprintf("paths.%s.src.client-id", consts.DefaultRelayerPath):     hubIbcConnection.ClientID,
						fmt.Sprintf("paths.%s.src.connection-id", consts.DefaultRelayerPath): hubIbcConnection.ID,

						// ra
						fmt.Sprintf("chains.%s.value.gas-adjustment", raResponse.Rollapp.RollappId): 1.3,
						fmt.Sprintf("chains.%s.value.is-dym-rollapp", raResponse.Rollapp.RollappId): true,
						fmt.Sprintf(
							"paths.%s.dst.client-id",
							consts.DefaultRelayerPath,
						): rollappIbcConnection.ClientID,
						fmt.Sprintf("paths.%s.dst.connection-id", consts.DefaultRelayerPath): rollappIbcConnection.ID,

						// misc
						"extra-codecs": []string{
							"ethermint",
						},
					}
					err = yamlconfig.UpdateNestedYAML(relayerConfigPath, updates)
					if err != nil {
						pterm.Error.Printf("Error updating YAML: %v\n", err)
						return
					}
				}
				pterm.Info.Println("existing IBC channels found ")
				pterm.Info.Println("Hub: ", srcIbcChannel)
				pterm.Info.Println("RollApp: ", dstIbcChannel)
				return
			}

			// TODO: remove code duplication
			_, err = os.Stat(relayerHome)
			if err != nil {
				if err != nil {
					pterm.Error.Printf("failed to create %s: %v\n", relayerHome, err)
					return
				}
			}

			pterm.Info.Println("initializing relayer config")
			err = initconfig.InitializeRelayerConfig(
				relayer.ChainConfig{
					ID:            raData.ID,
					RPC:           raData.RpcUrl,
					Denom:         rollappChainData.Denom,
					AddressPrefix: rollappChainData.Bech32Prefix,
					GasPrices:     "2000000000",
				}, relayer.ChainConfig{
					ID:            rollappChainData.HubData.ID,
					RPC:           rollappChainData.HubData.RPC_URL,
					Denom:         consts.Denoms.Hub,
					AddressPrefix: consts.AddressPrefixes.Hub,
					GasPrices:     rollappChainData.HubData.GAS_PRICE,
				}, home,
			)
			if err != nil {
				pterm.Error.Printf(
					"failed to initialize relayer config: %v\n",
					err,
				)
				return
			}

			rlyKeys, err := keys.GenerateRelayerKeys(*rollappChainData)
			if err != nil {
				pterm.Error.Printf("failed to create relayer keys: %v\n", err)
				return
			}
			for _, key := range rlyKeys {
				key.Print(keys.WithMnemonic(), keys.WithName())
			}

			keysToFund, err := keys.GetRelayerKeys(*rollappChainData)
			if err != nil {
				pterm.Error.Println("failed to retrieve relayer keys: ", err)
				return
			}
			pterm.Info.Println(
				"please fund the hub relayer key with at least 20 dym tokens: ",
			)
			for _, k := range keysToFund {
				k.Print(keys.WithName())
			}
			proceed, _ := pterm.DefaultInteractiveConfirm.WithDefaultValue(false).
				WithDefaultText(
					"press 'y' when the wallets are funded",
				).Show()
			if !proceed {
				return
			}

			if err := relayer.CreatePath(*rollappChainData); err != nil {
				pterm.Error.Printf("failed to create relayer IBC path: %v\n", err)
				return
			}

			rollappIbcConnection, hubIbcConnection, err := rly.GetActiveConnections(raData, hd)
			if err != nil {
				pterm.Error.Printf("failed to retrieve active connections: %v\n", err)
				return
			}

			pterm.Info.Println("updating application relayer config")
			relayerConfigPath := filepath.Join(relayerHome, "config", "config.yaml")
			updates := map[string]interface{}{
				fmt.Sprintf("chains.%s.value.gas-adjustment", rollappChainData.HubData.ID): 1.5,
				fmt.Sprintf("chains.%s.value.gas-adjustment", rollappChainData.RollappID):  1.3,
				fmt.Sprintf("chains.%s.value.is-dym-hub", rollappChainData.HubData.ID):     true,
				fmt.Sprintf(
					"chains.%s.value.http-addr",
					rollappChainData.HubData.ID,
				): rollappChainData.HubData.API_URL,
				fmt.Sprintf("chains.%s.value.is-dym-rollapp", rollappChainData.RollappID): true,
				"extra-codecs": []string{
					"ethermint",
				},
				fmt.Sprintf("paths.%s.dst.client-id", consts.DefaultRelayerPath):     rollappIbcConnection.ClientID,
				fmt.Sprintf("paths.%s.dst.connection-id", consts.DefaultRelayerPath): rollappIbcConnection.ID,
				fmt.Sprintf("paths.%s.src.client-id", consts.DefaultRelayerPath):     hubIbcConnection.ClientID,
				fmt.Sprintf("paths.%s.src.connection-id", consts.DefaultRelayerPath): hubIbcConnection.ID,
			}
			err = yamlconfig.UpdateNestedYAML(relayerConfigPath, updates)
			if err != nil {
				pterm.Error.Printf("Error updating YAML: %v\n", err)
				return
			}

			fmt.Println("hub channel: ", srcIbcChannel)
			fmt.Println("ra channel: ", dstIbcChannel)
		},
	}

	relayerStartCmd.Flags().
		BoolP(flagOverride, "", false, "override the existing relayer clients and channels")
	return relayerStartCmd
}

func VerifyRelayerBalances(raData consts.RollappData, hd consts.HubData) error {
	insufficientBalances, err := relayer.GetRelayerInsufficientBalances(raData, hd)
	if err != nil {
		return err
	}

	if len(insufficientBalances) != 0 {
		err = keys.PrintInsufficientBalancesIfAny(insufficientBalances)
		if err != nil {
			return err
		}
	}

	return nil
}
