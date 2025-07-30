package commands

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/erigontech/erigon-lib/common/background"
	"github.com/erigontech/erigon-lib/common/datadir"
	"github.com/erigontech/erigon-lib/config3"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/recsplit"
	"github.com/erigontech/erigon-lib/recsplit/eliasfano32"
	"github.com/erigontech/erigon-lib/recsplit/multiencseq"
	"github.com/erigontech/erigon-lib/state"

	"path/filepath"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/seg"
	"github.com/erigontech/erigon/turbo/debug"
	"github.com/spf13/cobra"
)

// TODO: this utility can be safely deleted after PR https://github.com/erigontech/erigon/pull/12907/ is rolled out in production
func parseEFFilename(fileName string) (*efFileInfo, error) {
	// Handle new format: v1.0-code.0-64.ef
	partsByDot := strings.Split(fileName, ".")
	if len(partsByDot) < 4 {
		return nil, fmt.Errorf("invalid filename format: %s", fileName)
	}

	// Step range is in the second-to-last part (before .ef)
	stepRangePart := partsByDot[len(partsByDot)-2] // "0-64"
	stepParts := strings.Split(stepRangePart, "-")
	if len(stepParts) != 2 {
		return nil, fmt.Errorf("invalid step range: %s", stepRangePart)
	}

	startStep, err := strconv.ParseUint(stepParts[0], 10, 64)
	if err != nil {
		return nil, err
	}
	endStep, err := strconv.ParseUint(stepParts[1], 10, 64)
	if err != nil {
		return nil, err
	}

	// Prefix is everything before the step range
	prefix := strings.Join(partsByDot[:len(partsByDot)-2], ".")

	return &efFileInfo{
		prefix:    prefix,
		stepSize:  endStep - startStep,
		startStep: startStep,
		endStep:   endStep,
	}, nil
}

type efFileInfo struct {
	prefix    string
	stepSize  uint64
	startStep uint64
	endStep   uint64
}

var b []byte

func doConvert(baseTxNum uint64, v []byte) ([]byte, error) {
	ef, _ := eliasfano32.ReadEliasFano(v)

	seqBuilder := multiencseq.NewBuilder(baseTxNum, ef.Count(), ef.Max())
	for it := ef.Iterator(); it.HasNext(); {
		n, err := it.Next()
		if err != nil {
			return nil, err
		}
		seqBuilder.AddOffset(n)
	}
	seqBuilder.Build()

	b = seqBuilder.AppendBytes(b[:0])
	return b, nil
}

var idxOptimize = &cobra.Command{
	Use:   "idx_optimize",
	Short: "Scan .ef files, backup them up, reencode and optimize them, rebuild .efi files",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, _ := common.RootContext()
		logger := debug.SetupCobra(cmd, "integration")
		dirs := datadir.New(datadirCli)

		if err := CheckSaltFilesExist(dirs); err != nil {
			logger.Error("Failed to check salt files", "error", err)
			return
		}

		// accessorDir := filepath.Join(datadirCli, "snapshots", "accessor")
		idxPath := dirs.SnapIdx
		idxDir := os.DirFS(idxPath)

		files, err := fs.ReadDir(idxDir, ".")
		if err != nil {
			logger.Error("Failed to read directory contents", "error", err)
			return
		}

		logger.Info("Sumarizing idx files...")
		cEF := 0
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".ef") {
				continue
			}
			cEF++
		}

		logger.Info("Optimizing idx files...")
		cOpt := 0
		skipped := 0
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".ef") {
				continue
			}

			// Check if file has already been optimized and skip if requested
			if _, err := os.Stat(filepath.Join(dirs.SnapIdx, file.Name()+".new")); err == nil {
				logger.Info("Skipping already optimized file", "file", file.Name())
				skipped++
				continue
			}

			efInfo, err := parseEFFilename(file.Name())
			if err != nil {
				logger.Error("Failed to parse file info: ", err)
				continue
			}
			logger.Info("Optimizing...", "file", file.Name(), "n", cOpt, "total", cEF)

			cOpt++
			baseTxNum := efInfo.startStep * config3.DefaultStepSize

			tmpDir := dirs.Tmp

			idxInput, err := seg.NewDecompressor(filepath.Join(dirs.SnapIdx, file.Name()))
			if err != nil {
				logger.Error("Failed to open decompressor", "error", err)
				return
			}
			defer idxInput.Close()

			idxOutput, err := seg.NewCompressor(ctx, "optimizoor", filepath.Join(dirs.SnapIdx, file.Name()+".new"), tmpDir, seg.DefaultCfg, log.LvlInfo, logger)
			if err != nil {
				logger.Error("Failed to open compressor", "error", err)
				return
			}
			defer idxOutput.Close()

			// Summarize 1 idx file
			g := idxInput.MakeGetter()
			reader := seg.NewReader(g, seg.CompressNone)
			reader.Reset(0)

			writer := seg.NewWriter(idxOutput, seg.CompressNone)
			ps := background.NewProgressSet()

			logger.Info("Starting data processing", "file", file.Name())
			processedItems := 0

			for reader.HasNext() {
				k, _ := reader.Next(nil)
				if !reader.HasNext() {
					logger.Error("reader doesn't have next!")
					return
				}
				if err := writer.AddWord(k); err != nil {
					logger.Error("error while writing key", "error", err)
				}

				v, _ := reader.Next(nil)
				v, err := doConvert(baseTxNum, v)
				if err != nil {
					logger.Error("error while optimizing value", "error", err)
					return
				}
				if err := writer.AddWord(v); err != nil {
					logger.Error("error while writing value", "error", err)
					return
				}

				processedItems++
				// Log progress every 10000 items
				if processedItems%10000 == 0 {
					logger.Info("Processing progress", "file", file.Name(), "items", processedItems)
				}

				select {
				case <-ctx.Done():
					return
				default:
				}
			}

			logger.Info("Data processing completed", "file", file.Name(), "total_items", processedItems)
			if err := writer.Compress(); err != nil {
				logger.Error("error while writing optimized file", "error", err)
				return
			}
			idxInput.Close()
			writer.Close()
			idxOutput.Close()

			logger.Info("Optimization completed, starting index rebuild", "file", file.Name())

			// rebuid .efi; COPIED FROM InvertedIndex.buildMapAccessor
			logger.Info("Getting salt for index generation", "file", file.Name())
			salt, err := state.GetStateIndicesSalt(dirs, false, logger)
			if err != nil {
				logger.Error("Failed to build accessor", "error", err)
				return
			}
			logger.Info("Salt obtained successfully", "file", file.Name())

			idxPath := filepath.Join(dirs.SnapAccessors, file.Name()+"i.new")
			logger.Info("Building hash map accessor", "file", file.Name(), "output", idxPath)

			cfg := recsplit.RecSplitArgs{
				Version:            1,
				Enums:              true,
				LessFalsePositives: true,

				BucketSize: recsplit.DefaultBucketSize,
				LeafSize:   recsplit.DefaultLeafSize,
				TmpDir:     tmpDir,
				IndexFile:  idxPath,
				Salt:       salt,
				NoFsync:    false,
			}

			logger.Info("Opening optimized file for index generation", "file", file.Name()+".new")
			data, err := seg.NewDecompressor(filepath.Join(dirs.SnapIdx, file.Name()+".new"))
			if err != nil {
				logger.Error("Failed to build accessor", "error", err)
				return
			}

			logger.Info("Starting hash map accessor build", "file", file.Name())
			if err := state.BuildHashMapAccessor(ctx, seg.NewReader(data.MakeGetter(), seg.CompressNone), idxPath, false, cfg, ps, logger); err != nil {
				logger.Error("Failed to build accessor", "error", err)
				return
			}
			data.Close()

			// Log file sizes for comparison
			if originalInfo, err := os.Stat(filepath.Join(dirs.SnapIdx, file.Name())); err == nil {
				if newInfo, err := os.Stat(filepath.Join(dirs.SnapIdx, file.Name()+".new")); err == nil {
					originalSize := originalInfo.Size()
					newSize := newInfo.Size()
					compressionRatio := float64(newSize) / float64(originalSize) * 100
					logger.Info("File optimization completed",
						"file", file.Name(),
						"original_size", originalSize,
						"new_size", newSize,
						"compression_ratio", fmt.Sprintf("%.2f%%", compressionRatio))
				}
			}

			if accessorInfo, err := os.Stat(idxPath); err == nil {
				logger.Info("Index file generated",
					"file", file.Name()+"i.new",
					"size", accessorInfo.Size())
			}
		}

		logger.Info(fmt.Sprintf("Optimized %d of %d files!!!", cOpt, cEF))
	},
}

func init() {
	withDataDir(idxOptimize)
	rootCmd.AddCommand(idxOptimize)
}
