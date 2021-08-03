package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/claimtrie"
	"github.com/btcsuite/btcd/claimtrie/chain/chainrepo"
	"github.com/btcsuite/btcd/claimtrie/change"
	"github.com/btcsuite/btcd/claimtrie/config"
	"github.com/btcsuite/btcd/database"
	_ "github.com/btcsuite/btcd/database/ffldb"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(NewChainCommands())
}

func NewChainCommands() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "chain",
		Short: "chain related command",
	}

	cmd.AddCommand(NewChainDumpCommand())
	cmd.AddCommand(NewChainReplayCommand())
	cmd.AddCommand(NewChainConvertCommand())

	return cmd
}

func NewChainDumpCommand() *cobra.Command {

	var fromHeight int32
	var toHeight int32

	cmd := &cobra.Command{
		Use:   "dump",
		Short: "Dump the chain changes between <fromHeight> and <toHeight>",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {

			dbPath := filepath.Join(dataDir, netName, "claim_dbs", cfg.ChainRepoPebble.Path)
			log.Debugf("Open chain repo: %q", dbPath)
			chainRepo, err := chainrepo.NewPebble(dbPath)
			if err != nil {
				return errors.Wrapf(err, "open chain repo")
			}

			for height := fromHeight; height <= toHeight; height++ {
				changes, err := chainRepo.Load(height)
				if errors.Is(err, pebble.ErrNotFound) {
					continue
				}
				if err != nil {
					return errors.Wrapf(err, "load charnges for height: %d")
				}
				for _, chg := range changes {
					showChange(chg)
				}
			}

			return nil
		},
	}

	cmd.Flags().Int32Var(&fromHeight, "from", 0, "From height (inclusive)")
	cmd.Flags().Int32Var(&toHeight, "to", 0, "To height (inclusive)")
	cmd.Flags().SortFlags = false

	return cmd
}

func NewChainReplayCommand() *cobra.Command {

	var fromHeight int32
	var toHeight int32

	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay the chain changes between <fromHeight> and <toHeight>",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {

			for _, dbName := range []string{
				cfg.BlockRepoPebble.Path,
				cfg.NodeRepoPebble.Path,
				cfg.MerkleTrieRepoPebble.Path,
				cfg.TemporalRepoPebble.Path,
			} {
				dbPath := filepath.Join(dataDir, netName, "claim_dbs", dbName)
				log.Debugf("Delete repo: %q", dbPath)
				err := os.RemoveAll(dbPath)
				if err != nil {
					return errors.Wrapf(err, "delete repo: %q", dbPath)
				}
			}

			dbPath := filepath.Join(dataDir, netName, "claim_dbs", cfg.ChainRepoPebble.Path)
			log.Debugf("Open chain repo: %q", dbPath)
			chainRepo, err := chainrepo.NewPebble(dbPath)
			if err != nil {
				return errors.Wrapf(err, "open chain repo")
			}

			cfg := config.DefaultConfig
			cfg.RamTrie = true
			cfg.DataDir = filepath.Join(dataDir, netName)

			ct, err := claimtrie.New(cfg)
			if err != nil {
				return errors.Wrapf(err, "create claimtrie")
			}
			defer ct.Close()

			db, err := loadBlocksDB()
			if err != nil {
				return errors.Wrapf(err, "load blocks database")
			}

			chain, err := loadChain(db)
			if err != nil {
				return errors.Wrapf(err, "load chain")
			}

			for ht := fromHeight; ht < toHeight; ht++ {

				changes, err := chainRepo.Load(ht + 1)
				if errors.Is(err, pebble.ErrNotFound) {
					// do nothing.
				} else if err != nil {
					return errors.Wrapf(err, "load changes for block %d", ht)
				}

				for _, chg := range changes {

					switch chg.Type {
					case change.AddClaim:
						err = ct.AddClaim(chg.Name, chg.OutPoint, chg.ClaimID, chg.Amount)
					case change.UpdateClaim:
						err = ct.UpdateClaim(chg.Name, chg.OutPoint, chg.Amount, chg.ClaimID)
					case change.SpendClaim:
						err = ct.SpendClaim(chg.Name, chg.OutPoint, chg.ClaimID)
					case change.AddSupport:
						err = ct.AddSupport(chg.Name, chg.OutPoint, chg.Amount, chg.ClaimID)
					case change.SpendSupport:
						err = ct.SpendSupport(chg.Name, chg.OutPoint, chg.ClaimID)
					default:
						err = errors.Errorf("invalid change type: %v", chg)
					}

					if err != nil {
						return errors.Wrapf(err, "execute change %v", chg)
					}
				}
				err = appendBlock(ct, chain)
				if err != nil {
					return errors.Wrapf(err, "appendBlock")
				}

				if ct.Height()%1000 == 0 {
					fmt.Printf("block: %d\n", ct.Height())
				}
			}

			return nil
		},
	}

	// FIXME
	cmd.Flags().Int32Var(&fromHeight, "from", 0, "From height")
	cmd.Flags().Int32Var(&toHeight, "to", 0, "To height")
	cmd.Flags().SortFlags = false

	return cmd
}

func appendBlock(ct *claimtrie.ClaimTrie, chain *blockchain.BlockChain) error {

	err := ct.AppendBlock()
	if err != nil {
		return errors.Wrapf(err, "append block: %w")
	}

	block, err := chain.BlockByHeight(ct.Height())
	if err != nil {
		return errors.Wrapf(err, "load from block repo: %w")
	}
	hash := block.MsgBlock().Header.ClaimTrie

	if *ct.MerkleHash() != hash {
		return errors.Errorf("hash mismatched at height %5d: exp: %s, got: %s", ct.Height(), hash, ct.MerkleHash())
	}

	return nil
}

func NewChainConvertCommand() *cobra.Command {

	var height int32

	cmd := &cobra.Command{
		Use:   "convert",
		Short: "convert changes from to <height>",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {

			db, err := loadBlocksDB()
			if err != nil {
				return errors.Wrapf(err, "load block db")
			}
			defer db.Close()

			chain, err := loadChain(db)
			if err != nil {
				return errors.Wrapf(err, "load block db")
			}

			converter := chainConverter{
				db:          db,
				chain:       chain,
				blockChan:   make(chan *btcutil.Block, 1000),
				changesChan: make(chan []change.Change, 1000),
				wg:          &sync.WaitGroup{},
				stat:        &stat{},
			}

			startTime := time.Now()
			err = converter.start()
			if err != nil {
				return errors.Wrapf(err, "start Converter")
			}

			converter.wait()
			log.Infof("Convert chain: took %s", time.Since(startTime))

			return nil
		},
	}

	cmd.Flags().Int32Var(&height, "height", 0, "Height")

	return cmd
}

type stat struct {
	blocksFetched   int
	blocksProcessed int
	changesSaved    int
}

type chainConverter struct {
	db    database.DB
	chain *blockchain.BlockChain

	blockChan   chan *btcutil.Block
	changesChan chan []change.Change

	wg *sync.WaitGroup

	stat *stat
}

func (cc *chainConverter) wait() {
	cc.wg.Wait()
}

func (cb *chainConverter) start() error {

	go cb.reportStats()

	cb.wg.Add(3)
	go cb.getBlock()
	go cb.processBlock()
	go cb.saveChanges()

	return nil
}

func (cb *chainConverter) getBlock() {
	defer cb.wg.Done()
	defer close(cb.blockChan)

	toHeight := int32(200000)
	fmt.Printf("blocks: %d\n", cb.chain.BestSnapshot().Height)

	if toHeight > cb.chain.BestSnapshot().Height {
		toHeight = cb.chain.BestSnapshot().Height

	}

	for ht := int32(0); ht < toHeight; ht++ {
		block, err := cb.chain.BlockByHeight(ht)
		if err != nil {
			log.Errorf("load changes from repo: %w", err)
			return
		}
		cb.stat.blocksFetched++
		cb.blockChan <- block
	}
}

func (cb *chainConverter) processBlock() {
	defer cb.wg.Done()
	defer close(cb.changesChan)

	view := blockchain.NewUtxoViewpoint()
	for block := range cb.blockChan {
		var changes []change.Change
		for _, tx := range block.Transactions() {
			view.AddTxOuts(tx, block.Height())

			if blockchain.IsCoinBase(tx) {
				continue
			}

			for _, txIn := range tx.MsgTx().TxIn {
				op := txIn.PreviousOutPoint
				e := view.LookupEntry(op)
				if e == nil {
					log.Criticalf("Missing input in view for %s", op.String())
				}
				cs, err := txscript.DecodeClaimScript(e.PkScript())
				if err == txscript.ErrNotClaimScript {
					continue
				}
				if err != nil {
					log.Criticalf("Can't parse claim script: %s", err)
				}

				chg := change.Change{
					Height:   block.Height(),
					Name:     cs.Name(),
					OutPoint: txIn.PreviousOutPoint,
				}

				switch cs.Opcode() {
				case txscript.OP_CLAIMNAME:
					chg.Type = change.SpendClaim
					chg.ClaimID = change.NewClaimID(chg.OutPoint)
				case txscript.OP_UPDATECLAIM:
					chg.Type = change.SpendClaim
					copy(chg.ClaimID[:], cs.ClaimID())
				case txscript.OP_SUPPORTCLAIM:
					chg.Type = change.SpendSupport
					copy(chg.ClaimID[:], cs.ClaimID())
				}

				changes = append(changes, chg)
			}

			op := *wire.NewOutPoint(tx.Hash(), 0)
			for i, txOut := range tx.MsgTx().TxOut {
				cs, err := txscript.DecodeClaimScript(txOut.PkScript)
				if err == txscript.ErrNotClaimScript {
					continue
				}

				op.Index = uint32(i)
				chg := change.Change{
					Height:   block.Height(),
					Name:     cs.Name(),
					OutPoint: op,
					Amount:   txOut.Value,
				}

				switch cs.Opcode() {
				case txscript.OP_CLAIMNAME:
					chg.Type = change.AddClaim
					chg.ClaimID = change.NewClaimID(op)
				case txscript.OP_SUPPORTCLAIM:
					chg.Type = change.AddSupport
					copy(chg.ClaimID[:], cs.ClaimID())
				case txscript.OP_UPDATECLAIM:
					chg.Type = change.UpdateClaim
					copy(chg.ClaimID[:], cs.ClaimID())
				}
				changes = append(changes, chg)
			}
		}
		cb.stat.blocksProcessed++

		if len(changes) != 0 {
			cb.changesChan <- changes
		}
	}
}

func (cb *chainConverter) saveChanges() {
	defer cb.wg.Done()

	dbPath := filepath.Join(dataDir, netName, "claim_dbs", cfg.ChainRepoPebble.Path)
	chainRepo, err := chainrepo.NewPebble(dbPath)
	if err != nil {
		log.Errorf("open chain repo: %s", err)
		return
	}
	defer chainRepo.Close()

	for changes := range cb.changesChan {
		err = chainRepo.Save(changes[0].Height, changes)
		if err != nil {
			log.Errorf("save to chain repo: %s", err)
			return
		}
		cb.stat.changesSaved++
	}
}

func (cb *chainConverter) reportStats() {
	stat := cb.stat
	tick := time.NewTicker(5 * time.Second)
	for range tick.C {
		log.Infof("block : %7d / %7d,  changes saved: %d",
			stat.blocksFetched, stat.blocksProcessed, stat.changesSaved)

	}
}
