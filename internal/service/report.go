package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/dchest/uniuri"
	"github.com/go-redis/redis/v8"
	"github.com/gofiber/fiber/v2"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"

	"github.com/penguin-statistics/backend-next/internal/constant"
	"github.com/penguin-statistics/backend-next/internal/model/types"
	"github.com/penguin-statistics/backend-next/internal/pkg/pgerr"
	"github.com/penguin-statistics/backend-next/internal/pkg/pgid"
	"github.com/penguin-statistics/backend-next/internal/repo"
	"github.com/penguin-statistics/backend-next/internal/util"
	"github.com/penguin-statistics/backend-next/internal/util/reportutil"
	"github.com/penguin-statistics/backend-next/internal/util/reportverifs"
)

var (
	ErrReportNotFound = pgerr.ErrInvalidReq.Msg("report not existed or has already been recalled")
	ErrNatsTimeout    = errors.New("timeout waiting for NATS response")
)

type Report struct {
	DB                     *bun.DB
	Redis                  *redis.Client
	NatsJS                 nats.JetStreamContext
	ItemService            *Item
	StageService           *Stage
	AccountService         *Account
	StageRepo              *repo.Stage
	DropInfoRepo           *repo.DropInfo
	DropReportRepo         *repo.DropReport
	DropPatternRepo        *repo.DropPattern
	DropReportExtraRepo    *repo.DropReportExtra
	DropPatternElementRepo *repo.DropPatternElement
	ReportVerifier         *reportverifs.ReportVerifiers
}

func NewReport(db *bun.DB, redisClient *redis.Client, natsJs nats.JetStreamContext, itemService *Item, stageService *Stage, stageRepo *repo.Stage, dropInfoRepo *repo.DropInfo, dropReportRepo *repo.DropReport, dropReportExtraRepo *repo.DropReportExtra, dropPatternRepo *repo.DropPattern, dropPatternElementRepo *repo.DropPatternElement, accountService *Account, reportVerifier *reportverifs.ReportVerifiers) *Report {
	service := &Report{
		DB:                     db,
		Redis:                  redisClient,
		NatsJS:                 natsJs,
		ItemService:            itemService,
		StageService:           stageService,
		AccountService:         accountService,
		StageRepo:              stageRepo,
		DropInfoRepo:           dropInfoRepo,
		DropReportRepo:         dropReportRepo,
		DropPatternRepo:        dropPatternRepo,
		DropReportExtraRepo:    dropReportExtraRepo,
		DropPatternElementRepo: dropPatternElementRepo,
		ReportVerifier:         reportVerifier,
	}
	return service
}

func (s *Report) pipelineAccount(ctx *fiber.Ctx) (accountId int, err error) {
	account, err := s.AccountService.GetAccountFromRequest(ctx)
	if err != nil {
		createdAccount, err := s.AccountService.CreateAccountWithRandomPenguinId(ctx.Context())
		if err != nil {
			return 0, err
		}
		accountId = createdAccount.AccountID
		pgid.Inject(ctx, createdAccount.PenguinID)
	} else {
		accountId = account.AccountID
	}

	return accountId, nil
}

func (s *Report) pipelineMergeDropsAndMapDropTypes(ctx context.Context, drops []types.ArkDrop) ([]*types.Drop, error) {
	drops = reportutil.MergeDropsByDropTypeAndItemID(drops)

	convertedDrops := make([]*types.Drop, 0, len(drops))
	for _, drop := range drops {
		item, err := s.ItemService.GetItemByArkId(ctx, drop.ItemID)
		if err != nil {
			if !errors.Is(err, pgerr.ErrNotFound) {
				return nil, err
			} else {
				log.Warn().Msgf("failed to get item by ark id '%s', will ignore it", drop.ItemID)
				continue
			}
		}

		convertedDrops = append(convertedDrops, &types.Drop{
			// maps DropType to DB DropType
			DropType: constant.DropTypeMap[drop.DropType],
			ItemID:   item.ItemID,
			Quantity: drop.Quantity,
		})
	}

	return convertedDrops, nil
}

func (s *Report) pipelineTaskId(ctx *fiber.Ctx) string {
	return ctx.Locals(constant.ContextKeyRequestID).(string) + "-" + uniuri.NewLen(16)
}

func (s *Report) pipelineAggregateGachaboxDrops(ctx context.Context, singleReport *types.ReportTaskSingleReport) error {
	// for gachabox drop, we need to aggregate `times` according to `quantity` for report.Drops
	category, err := s.StageService.GetStageExtraProcessTypeByArkId(ctx, singleReport.StageID)
	if err != nil {
		return err
	}
	if category.Valid && category.String == constant.ExtraProcessTypeGachaBox {
		reportutil.AggregateGachaBoxDrops(singleReport)
	}

	return nil
}

// FIXME: temporary compensation for reports from MaaAssistant, where stageId passed for act18d3 is currently ambiguous
// this function will mutate req with the correct stageId, if detected that such request matches the following criteria:
// 1. report time < 1654718400000
// 2. is from MeoAssistant
// 3. stageId is in form `act18d3_0$_perm` where $ represents integers [1-9]
func (s *Report) pipelineMaaAct18d3TemporaryMitigation(ctx *fiber.Ctx, req *types.SingleReportRequest) {
	if time.Now().UnixMilli() < 1654718400000 && req.Source == "MeoAssistant" {
		if strings.HasPrefix(req.StageID, "act18d3_") && strings.HasSuffix(req.StageID, "_perm") {
			req.StageID = strings.Replace(req.StageID, "_perm", "_rep", 1)
		}
	}
}

func (s *Report) commitReportTask(ctx *fiber.Ctx, subject string, task *types.ReportTask) (taskId string, err error) {
	taskId = s.pipelineTaskId(ctx)
	task.TaskID = taskId

	reportTaskJSON, err := json.Marshal(task)
	if err != nil {
		return "", err
	}

	pub, err := s.NatsJS.PublishAsync(subject, reportTaskJSON)
	if err != nil {
		return "", err
	}

	select {
	case err := <-pub.Err():
		return "", err
	case <-pub.Ok():
		return taskId, nil
	case <-ctx.Context().Done():
		return "", ctx.Context().Err()
	case <-time.After(time.Second * 10):
		return "", ErrNatsTimeout
	}
}

// returns taskID and error, if any
func (s *Report) PreprocessAndQueueSingularReport(ctx *fiber.Ctx, req *types.SingleReportRequest) (taskId string, err error) {
	// if account is not found, create new account
	accountId, err := s.pipelineAccount(ctx)
	if err != nil {
		return "", err
	}

	// merge drops with same (dropType, itemId) pair
	drops, err := s.pipelineMergeDropsAndMapDropTypes(ctx.Context(), req.Drops)
	if err != nil {
		return "", err
	}

	singleReport := &types.ReportTaskSingleReport{
		FragmentStageID: req.FragmentStageID,
		Drops:           drops,
		// for now, we do not support multiple report by specifying `times`
		Times:    1,
		Metadata: req.Metadata,
	}

	// for gachabox drop, we need to aggregate `times` according to `quantity` for report.Drops
	err = s.pipelineAggregateGachaboxDrops(ctx.Context(), singleReport)
	if err != nil {
		return "", err
	}

	s.pipelineMaaAct18d3TemporaryMitigation(ctx, req)

	// construct ReportContext
	reportTask := &types.ReportTask{
		CreatedAt: time.Now().UnixMicro(),
		FragmentReportCommon: types.FragmentReportCommon{
			Server:  req.Server,
			Source:  req.Source,
			Version: req.Version,
		},
		Reports:   []*types.ReportTaskSingleReport{singleReport},
		AccountID: accountId,
		IP:        util.ExtractIP(ctx),
	}

	return s.commitReportTask(ctx, "REPORT.SINGLE", reportTask)
}

func (s *Report) PreprocessAndQueueBatchReport(ctx *fiber.Ctx, req *types.BatchReportRequest) (taskId string, err error) {
	// if account is not found, create new account
	accountId, err := s.pipelineAccount(ctx)
	if err != nil {
		return "", err
	}

	reports := make([]*types.ReportTaskSingleReport, len(req.BatchDrops))

	for i, drop := range req.BatchDrops {
		// merge drops with same (dropType, itemId) pair
		drops, err := s.pipelineMergeDropsAndMapDropTypes(ctx.Context(), drop.Drops)
		if err != nil {
			return "", err
		}

		// catch the variable
		metadata := drop.Metadata
		report := &types.ReportTaskSingleReport{
			FragmentStageID: drop.FragmentStageID,
			Drops:           drops,
			Times:           1,
			Metadata:        &metadata,
		}

		err = s.pipelineAggregateGachaboxDrops(ctx.Context(), report)
		if err != nil {
			return "", err
		}

		reports[i] = report
	}

	// construct ReportContext
	reportTask := &types.ReportTask{
		FragmentReportCommon: types.FragmentReportCommon{
			Server:  req.Server,
			Source:  req.Source,
			Version: req.Version,
		},
		Reports:   reports,
		AccountID: accountId,
		IP:        util.ExtractIP(ctx),
	}

	return s.commitReportTask(ctx, "REPORT.BATCH", reportTask)
}

func (s *Report) RecallSingularReport(ctx context.Context, req *types.SingleReportRecallRequest) error {
	var reportId int
	r := s.Redis.Get(ctx, req.ReportHash)

	if errors.Is(r.Err(), redis.Nil) {
		return ErrReportNotFound
	} else if r.Err() != nil {
		return r.Err()
	}

	reportId, err := r.Int()
	if err != nil {
		return err
	}

	err = s.DropReportRepo.DeleteDropReport(ctx, reportId)
	if err != nil {
		return err
	}

	s.Redis.Del(ctx, req.ReportHash)

	return nil
}
