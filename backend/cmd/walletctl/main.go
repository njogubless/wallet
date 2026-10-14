// Command walletctl is the operator CLI: migrations, staff role grants and
// on-demand reconciliation.
//
//	walletctl migrate up|down|status
//	walletctl grant-role <phone> <user|support|admin>
//	walletctl reconcile
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/phone"
	"github.com/sovereigntrust/wallet/internal/reconciliation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() error {
	return fmt.Errorf("usage: walletctl migrate up|down|status | grant-role <phone> <role> | reconcile")
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch args[0] {
	case "migrate":
		cmd := "up"
		if len(args) > 1 {
			cmd = args[1]
		}
		if err := db.MigrateCommand(ctx, pool, cmd); err != nil {
			return err
		}
		fmt.Println("migrate", cmd, "ok")
	case "grant-role":
		if len(args) != 3 {
			return usage()
		}
		ph, err := phone.NormalizeKE(args[1])
		if err != nil {
			return err
		}
		role := args[2]
		if role != "user" && role != "support" && role != "admin" {
			return fmt.Errorf("role must be user, support or admin")
		}
		tag, err := pool.Exec(ctx, `UPDATE users SET role = $2, updated_at = now() WHERE phone = $1`, ph, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("no user with phone %s", ph)
		}
		op := "operator-cli"
		if err := audit.RecordAs(ctx, pool, nil, &op, audit.Event{Action: "admin.role_granted", TargetType: "user_phone",
			TargetID: phone.Mask(ph), Details: map[string]any{"role": role}}); err != nil {
			return err
		}
		fmt.Printf("granted %s to %s\n", role, phone.Mask(ph))
	case "reconcile":
		run, err := (&reconciliation.Service{Pool: pool}).RunInternal(ctx, nil)
		if err != nil {
			return err
		}
		fmt.Printf("run %s %s %s\n", run.ID, run.Status, run.Stats)
	default:
		return usage()
	}
	return nil
}
