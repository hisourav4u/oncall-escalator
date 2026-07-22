package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"escalator/internal/bot"
	"escalator/internal/config"
	"escalator/internal/httpapi"
	"escalator/internal/slackapi"
	"escalator/internal/store"
)

func main() {
	// Bootstrap logger; level is refined from config once loaded.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(&logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(loggerp **slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Apply configured log level.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)
	*loggerp = logger

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	slack := slackapi.New(cfg.SlackBotToken)

	responders := bot.NewResponderCache(slack, cfg.ResponderUserIDs, cfg.ResponderEmails, cfg.ResponderUsergroupIDs, logger)
	oncall := bot.NewResponderCache(slack, nil, nil, cfg.WatchUsergroupIDs, logger)
	responders.Refresh(ctx) // prime before serving
	oncall.Refresh(ctx)
	go responders.RunRefresh(ctx, cfg.ResponderRefresh)
	go oncall.RunRefresh(ctx, cfg.ResponderRefresh)
	authorizer := bot.CombinedAuthorizer{Responders: responders, Oncall: oncall}

	targets := bot.NewTargetResolver(slack, cfg.EscalationTargets, logger)

	ackEmojis := map[string]bool{}
	ackAnyReplyUsers := map[string]bool{}
	for _, u := range cfg.AckAnyReplyUserIDs {
		ackAnyReplyUsers[u] = true
	}
	for _, e := range cfg.AckEmojis {
		ackEmojis[e] = true
	}
	nudgeExclude := map[string]bool{}
	for _, u := range cfg.NudgeExcludeUserIDs {
		nudgeExclude[u] = true
	}

	b := bot.New(bot.Config{
		WatchUsergroupIDs:    cfg.WatchUsergroupIDs,
		WatchKeywords:        cfg.WatchKeywords,
		TriggerTopLevelOnly:  cfg.TriggerTopLevelOnly,
		TriggerIgnoreLeading: cfg.TriggerIgnoreLeading,
		AckKeywords:          cfg.AckKeywords,
		AckEmojis:            ackEmojis,
		NudgeAfter:           cfg.NudgeAfter,
		EscalateAfter:        cfg.EscalateAfter,
		NudgeMode:            cfg.NudgeMode,
		NudgeExclude:         nudgeExclude,
		AckAnyOncallReply:    cfg.AckAnyOncallReply,
		AckAnyReplyUsers:     ackAnyReplyUsers,
		ReconcileInterval:    cfg.ReconcileInterval,
		ReconcileWindow:      cfg.ReconcileWindow,
		ReconcileChannels:    cfg.ReconcileChannels,
		EscalationMention:    targets.Mention,
	}, st, slack, authorizer, logger)

	go b.RunEscalations(ctx, cfg.TickInterval)
	go b.RunReconcile(ctx)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.New(b, st, slack, cfg.SlackSigningSecret, cfg.StatsToken, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.SlackSigningSecret == "" {
		logger.Warn("SLACK_SIGNING_SECRET not set — event signatures will NOT be verified")
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("escalator listening", "addr", srv.Addr,
			"watch", cfg.WatchUsergroupIDs, "nudge_after", cfg.NudgeAfter.String(),
			"escalate_after", cfg.EscalateAfter.String(), "nudge_mode", cfg.NudgeMode,
			"targets", cfg.EscalationTargets)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
