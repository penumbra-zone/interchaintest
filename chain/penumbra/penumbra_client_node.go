package penumbra

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"
	"strings"

	"cosmossdk.io/math"
	"github.com/BurntSushi/toml"
	transfertypes "github.com/cosmos/ibc-go/v8/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v8/modules/core/02-client/types"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	assetv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/core/asset/v1alpha1"
	ibcv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/core/component/ibc/v1alpha1"
	shielded_poolv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/core/component/shielded_pool/v1alpha1"
	keysv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/core/keys/v1alpha1"
	numv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/core/num/v1alpha1"
	custodyv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/custody/v1alpha1"
	viewv1alpha1 "github.com/strangelove-ventures/interchaintest/v8/chain/penumbra/view/v1alpha1"
	"github.com/strangelove-ventures/interchaintest/v8/ibc"
	"github.com/strangelove-ventures/interchaintest/v8/internal/dockerutil"
	"github.com/strangelove-ventures/interchaintest/v8/testutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type PenumbraClientNode struct {
	log *zap.Logger

	KeyName      string
	Index        int
	VolumeName   string
	Chain        ibc.Chain
	TestName     string
	NetworkID    string
	DockerClient *client.Client
	Image        ibc.DockerImage

	address    []byte
	addrString string

	containerLifecycle *dockerutil.ContainerLifecycle

	// Set during StartContainer.
	hostGRPCPort string
}

func NewClientNode(
	ctx context.Context,
	log *zap.Logger,
	chain *PenumbraChain,
	keyName string,
	index int,
	testName string,
	image ibc.DockerImage,
	dockerClient *client.Client,
	networkID string,
	address []byte,
	addrString string,
) (*PenumbraClientNode, error) {
	p := &PenumbraClientNode{
		log:          log,
		KeyName:      keyName,
		Index:        index,
		Chain:        chain,
		TestName:     testName,
		Image:        image,
		DockerClient: dockerClient,
		NetworkID:    networkID,
		address:      address,
		addrString:   addrString,
	}

	p.containerLifecycle = dockerutil.NewContainerLifecycle(log, dockerClient, p.Name())

	tv, err := dockerClient.VolumeCreate(ctx, volumetypes.CreateOptions{
		Labels: map[string]string{
			dockerutil.CleanupLabel:   testName,
			dockerutil.NodeOwnerLabel: p.Name(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating pclientd volume: %w", err)
	}
	p.VolumeName = tv.Name
	if err := dockerutil.SetVolumeOwner(ctx, dockerutil.VolumeOwnerOptions{
		Log: log,

		Client: dockerClient,

		VolumeName: p.VolumeName,
		ImageRef:   image.Ref(),
		TestName:   testName,
		UidGid:     image.UidGid,
	}); err != nil {
		return nil, fmt.Errorf("set pclientd volume owner: %w", err)
	}

	return p, nil
}

const (
	pclientdPort = "8081/tcp"
)

var pclientdPorts = nat.PortSet{
	nat.Port(pclientdPort): {},
}

// Name of the test node container
func (p *PenumbraClientNode) Name() string {
	return fmt.Sprintf("pclientd-%d-%s-%s-%s", p.Index, p.KeyName, p.Chain.Config().ChainID, p.TestName)
}

// the hostname of the test node container
func (p *PenumbraClientNode) HostName() string {
	return dockerutil.CondenseHostName(p.Name())
}

// Bind returns the home folder bind point for running the node
func (p *PenumbraClientNode) Bind() []string {
	return []string{fmt.Sprintf("%s:%s", p.VolumeName, p.HomeDir())}
}

func (p *PenumbraClientNode) HomeDir() string {
	return "/home/pclientd"
}

// GetAddress returns the Bech32m encoded string of the inner bytes as a slice of bytes.
func (p *PenumbraClientNode) GetAddress(ctx context.Context) ([]byte, error) {
	channel, err := grpc.Dial(p.hostGRPCPort, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer channel.Close()

	addrReq := &viewv1alpha1.AddressByIndexRequest{
		AddressIndex: &keysv1alpha1.AddressIndex{
			Account: 0,
		},
		// DisplayConfirm: true,
	}

	viewClient := viewv1alpha1.NewViewProtocolServiceClient(channel)

	resp, err := viewClient.AddressByIndex(ctx, addrReq)
	if err != nil {
		return nil, err
	}

	return resp.Address.Inner, nil
}

func (p *PenumbraClientNode) SendFunds(ctx context.Context, amount ibc.WalletAmount) error {
	channel, err := grpc.Dial(p.hostGRPCPort, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer channel.Close()

	hi, lo := translateBigInt(amount.Amount)

	// Generate a transaction plan sending funds to an address.
	tpr := &viewv1alpha1.TransactionPlannerRequest{
		WalletId: nil,
		Outputs: []*viewv1alpha1.TransactionPlannerRequest_Output{{
			Value: &assetv1alpha1.Value{
				Amount: &numv1alpha1.Amount{
					Lo: lo,
					Hi: hi,
				},
				AssetId: &assetv1alpha1.AssetId{AltBaseDenom: amount.Denom},
			},
			Address: &keysv1alpha1.Address{AltBech32M: amount.Address},
		}},
	}

	viewClient := viewv1alpha1.NewViewProtocolServiceClient(channel)

	resp, err := viewClient.TransactionPlanner(ctx, tpr)
	if err != nil {
		return err
	}

	// Get authorization data for the transaction from pclientd (signing).
	custodyClient := custodyv1alpha1.NewCustodyProtocolServiceClient(channel)
	authorizeReq := &custodyv1alpha1.AuthorizeRequest{
		Plan:              resp.Plan,
		WalletId:          &keysv1alpha1.WalletId{Inner: make([]byte, 32)},
		PreAuthorizations: []*custodyv1alpha1.PreAuthorization{},
	}

	authData, err := custodyClient.Authorize(ctx, authorizeReq)
	if err != nil {
		return err
	}

	// Have pclientd build and sign the planned transaction.
	wbr := &viewv1alpha1.WitnessAndBuildRequest{
		TransactionPlan:   resp.Plan,
		AuthorizationData: authData.Data,
	}

	tx, err := viewClient.WitnessAndBuild(ctx, wbr)
	if err != nil {
		return err
	}

	// Have pclientd broadcast and await confirmation of the built transaction.
	btr := &viewv1alpha1.BroadcastTransactionRequest{
		Transaction:    tx.Transaction,
		AwaitDetection: true,
	}

	_, err = viewClient.BroadcastTransaction(ctx, btr)
	if err != nil {
		return err
	}

	return nil
}

func (p *PenumbraClientNode) SendIBCTransfer(
	ctx context.Context,
	channelID string,
	amount ibc.WalletAmount,
	options ibc.TransferOptions,
) (ibc.Tx, error) {
	fmt.Println("In SendIBCTransfer from client perspective.")

	channel, err := grpc.Dial(
		p.hostGRPCPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return ibc.Tx{}, err
	}
	defer channel.Close()

	// TODO may need to be more defensive than this. additionally we may want to validate the addr string
	if p.addrString == "" {
		return ibc.Tx{}, fmt.Errorf("address string was not cached on pclientd instance for key with name %s", p.KeyName)
	}

	timeoutHeight, timeoutTimestamp := ibcTransferTimeouts(options)

	fmt.Println("Building Ics20Withdrawal...")
	hi, lo := translateBigInt(amount.Amount)

	withdrawal := &ibcv1alpha1.Ics20Withdrawal{
		Amount: &numv1alpha1.Amount{
			Lo: lo,
			Hi: hi,
		},
		Denom: &assetv1alpha1.Denom{
			Denom: amount.Denom,
		},
		DestinationChainAddress: amount.Address,
		ReturnAddress: &keysv1alpha1.Address{
			AltBech32M: p.addrString,
		},
		TimeoutHeight: &timeoutHeight,
		TimeoutTime:   timeoutTimestamp,
		SourceChannel: channelID,
	}

	// Generate a transaction plan sending ics_20 transfer
	tpr := &viewv1alpha1.TransactionPlannerRequest{
		WalletId:         nil,
		Ics20Withdrawals: []*ibcv1alpha1.Ics20Withdrawal{withdrawal},
	}

	viewClient := viewv1alpha1.NewViewProtocolServiceClient(channel)

	resp, err := viewClient.TransactionPlanner(ctx, tpr)
	if err != nil {
		return ibc.Tx{}, err
	}

	// Get authorization data for the transaction from pclientd (signing).
	custodyClient := custodyv1alpha1.NewCustodyProtocolServiceClient(channel)
	authorizeReq := &custodyv1alpha1.AuthorizeRequest{
		Plan:              resp.Plan,
		WalletId:          &keysv1alpha1.WalletId{Inner: make([]byte, 32)},
		PreAuthorizations: []*custodyv1alpha1.PreAuthorization{},
	}

	authData, err := custodyClient.Authorize(ctx, authorizeReq)
	if err != nil {
		return ibc.Tx{}, err
	}

	// Have pclientd build and sign the planned transaction.
	wbr := &viewv1alpha1.WitnessAndBuildRequest{
		TransactionPlan:   resp.Plan,
		AuthorizationData: authData.Data,
	}

	tx, err := viewClient.WitnessAndBuild(ctx, wbr)
	if err != nil {
		return ibc.Tx{}, err
	}

	// Have pclientd broadcast and await confirmation of the built transaction.
	btr := &viewv1alpha1.BroadcastTransactionRequest{
		Transaction:    tx.Transaction,
		AwaitDetection: true,
	}

	txResp, err := viewClient.BroadcastTransaction(ctx, btr)
	if err != nil {
		return ibc.Tx{}, err
	}

	// TODO: fill in rest of tx details
	return ibc.Tx{
		Height:   txResp.DetectionHeight,
		TxHash:   string(txResp.Id.Hash),
		GasSpent: 0,
		Packet: ibc.Packet{
			Sequence:         0,
			SourcePort:       "",
			SourceChannel:    "",
			DestPort:         "",
			DestChannel:      "",
			Data:             nil,
			TimeoutHeight:    "",
			TimeoutTimestamp: 0,
		},
	}, nil
}

func (p *PenumbraClientNode) GetBalance(ctx context.Context, denom string) (math.Int, error) {
	channel, err := grpc.Dial(
		p.hostGRPCPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return math.Int{}, err
	}
	defer channel.Close()

	viewClient := viewv1alpha1.NewViewProtocolServiceClient(channel)

	balanceRequest := &viewv1alpha1.BalancesRequest{
		AccountFilter: &keysv1alpha1.AddressIndex{
			Account: 0,
		},
		AssetIdFilter: &assetv1alpha1.AssetId{
			AltBaseDenom: denom,
		},
	}

	// The BalanceByAddress method returns a stream response, containing
	// zero-or-more balances, including denom and amount info per balance.
	balanceStream, err := viewClient.Balances(ctx, balanceRequest)
	if err != nil {
		return math.Int{}, err
	}

	var balances []*viewv1alpha1.BalancesResponse
	for {
		balance, err := balanceStream.Recv()
		if err != nil {
			// A gRPC streaming response will return EOF when it's done.
			if err == io.EOF {
				break
			} else {
				return math.Int{}, err
			}
		}
		balances = append(balances, balance)
	}

	if len(balances) <= 0 {
		return math.Int{}, fmt.Errorf("no balance was found for the denom %s", denom)
	}

	return translateHiAndLo(balances[0].Balance.Amount.Hi, balances[0].Balance.Amount.Lo), nil
}

// translateHiAndLo takes the high and low order bytes and decodes the two uint64 values into the single int128 value
// they represent. Since Go does not support native uint128 we make use of the big.Int type.
// see: https://github.com/penumbra-zone/penumbra/blob/4d175986f385e00638328c64d729091d45eb042a/crates/core/crypto/src/asset/amount.rs#L220-L240
func translateHiAndLo(hi, lo uint64) math.Int {
	hiBig := big.NewInt(0).SetUint64(hi)
	loBig := big.NewInt(0).SetUint64(lo)

	// Shift hi 8 bytes to the left
	hiBig.Lsh(hiBig, 64)

	// Add the lower order bytes
	i := big.NewInt(0).Add(hiBig, loBig)
	return math.NewIntFromBigInt(i)
}

// translateBigInt converts a Cosmos SDK Int, which is a wrapper around Go's big.Int, into two uint64 values
func translateBigInt(i math.Int) (uint64, uint64) {
	bz := i.BigInt().Bytes()

	// Pad the byte slice with leading zeros to ensure it's 16 bytes long
	paddedBytes := make([]byte, 16)
	copy(paddedBytes[16-len(bz):], bz)

	// Extract the high and low parts from the padded byte slice
	var hi uint64
	var lo uint64

	for j := 0; j < 8; j++ {
		hi <<= 8
		hi |= uint64(paddedBytes[j])
	}

	for j := 8; j < 16; j++ {
		lo <<= 8
		lo |= uint64(paddedBytes[j])
	}

	return hi, lo
}

// GetDenomMetadata invokes a gRPC request to obtain the DenomMetadata for a specified asset ID.
func (p *PenumbraClientNode) GetDenomMetadata(ctx context.Context, assetId *assetv1alpha1.AssetId) (*assetv1alpha1.DenomMetadata, error) {
	channel, err := grpc.Dial(
		p.hostGRPCPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	defer channel.Close()

	queryClient := shielded_poolv1alpha1.NewQueryServiceClient(channel)
	req := &shielded_poolv1alpha1.DenomMetadataByIdRequest{
		ChainId: p.Chain.Config().ChainID,
		AssetId: assetId,
	}

	resp, err := queryClient.DenomMetadataById(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp.DenomMetadata, nil
}

// WriteFile accepts file contents in a byte slice and writes the contents to
// the docker filesystem. relPath describes the location of the file in the
// docker volume relative to the home directory
func (p *PenumbraClientNode) WriteFile(ctx context.Context, content []byte, relPath string) error {
	fw := dockerutil.NewFileWriter(p.log, p.DockerClient, p.TestName)
	return fw.WriteFile(ctx, p.VolumeName, relPath, content)
}

// Initialize loads the view and spend keys into the pclientd config.
func (p *PenumbraClientNode) Initialize(ctx context.Context, pdAddress, spendKey, fullViewingKey string) error {
	c := make(testutil.Toml)

	kmsConfig := make(testutil.Toml)
	kmsConfig["spend_key"] = spendKey
	c["kms_config"] = kmsConfig
	c["full_viewing_key"] = fullViewingKey
	c["grpc_url"] = pdAddress
	c["bind_addr"] = "0.0.0.0:" + strings.Split(pclientdPort, "/")[0]

	buf := new(bytes.Buffer)
	if err := toml.NewEncoder(buf).Encode(c); err != nil {
		return err
	}

	return p.WriteFile(ctx, buf.Bytes(), "config.toml")
}

func (p *PenumbraClientNode) CreateNodeContainer(ctx context.Context) error {
	cmd := []string{
		"pclientd",
		"--home", p.HomeDir(),
		//"--node", pdAddress,
		"start",
		//"--bind-addr", "0.0.0.0:" + strings.Split(pclientdPort, "/")[0],
	}

	var env []string
	env = append(env, "RUST_LOG=debug")

	return p.containerLifecycle.CreateContainer(ctx, p.TestName, p.NetworkID, p.Image, pclientdPorts, p.Bind(), p.HostName(), cmd, env)
}

func (p *PenumbraClientNode) StopContainer(ctx context.Context) error {
	return p.containerLifecycle.StopContainer(ctx)
}

func (p *PenumbraClientNode) StartContainer(ctx context.Context) error {
	if err := p.containerLifecycle.StartContainer(ctx); err != nil {
		return err
	}

	hostPorts, err := p.containerLifecycle.GetHostPorts(ctx, pclientdPort)
	if err != nil {
		return err
	}

	p.hostGRPCPort = hostPorts[0]

	return nil
}

// Exec run a container for a specific job and block until the container exits
func (p *PenumbraClientNode) Exec(ctx context.Context, cmd []string, env []string) ([]byte, []byte, error) {
	job := dockerutil.NewImage(p.log, p.DockerClient, p.NetworkID, p.TestName, p.Image.Repository, p.Image.Version)
	opts := dockerutil.ContainerOptions{
		Binds: p.Bind(),
		Env:   env,
		User:  p.Image.UidGid,
	}
	res := job.Run(ctx, cmd, opts)
	return res.Stdout, res.Stderr, res.Err
}

// ibcTransferTimeouts returns a relative block height and timestamp timeout value to be used when sending an ics-20 transfer.
func ibcTransferTimeouts(options ibc.TransferOptions) (clienttypes.Height, uint64) {
	var (
		timeoutHeight    clienttypes.Height
		timeoutTimestamp uint64
	)

	// timeout is nil - use ics-20 defaults
	// timeout height and timestamp both set to 0 - use ics-20 defaults
	// timeout height and timestamp both have values greater than 0 - pass through values
	// timeout height or timestamp greater than 0 but other is zero - pass through values
	switch {
	case options.Timeout == nil:
		timeoutHeight, timeoutTimestamp = defaultTransferTimeouts()
	case options.Timeout.NanoSeconds == 0 && options.Timeout.Height == 0:
		timeoutHeight, timeoutTimestamp = defaultTransferTimeouts()
	default:
		timeoutTimestamp = options.Timeout.NanoSeconds
		timeoutHeight = clienttypes.NewHeight(0, options.Timeout.Height)
	}

	return timeoutHeight, timeoutTimestamp
}

// defaultTransferTimeouts returns the default relative timeout values from ics-20 for both block height and timestamp
// based timeouts.
// see: https://github.com/cosmos/ibc-go/blob/0364aae96f0326651c411ed0f3486be570280e5c/modules/apps/transfer/types/packet.go#L22-L33
func defaultTransferTimeouts() (clienttypes.Height, uint64) {
	t, err := clienttypes.ParseHeight(transfertypes.DefaultRelativePacketTimeoutHeight)
	if err != nil {
		panic(fmt.Errorf("cannot parse packet timeout height string when retrieving default value: %w", err))
	}
	return t, transfertypes.DefaultRelativePacketTimeoutTimestamp
}