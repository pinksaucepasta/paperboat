package machineevents

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

func TestHostIsToldAboutAChangeWithoutPollingOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	account, machine, other := "events_account_"+stamp, "events_machine_"+stamp, "events_other_"+stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	for index, id := range []string{machine, other} {
		exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,last_seen_at) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',now())`, id, account, "env_"+id, fmt.Sprintf("box-%d", index))
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	hub := NewHub(database.Pool(), database.Direct())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	time.Sleep(300 * time.Millisecond) // let the listener attach

	publish := func(ids []string, topic string) {
		t.Helper()
		tx, err := database.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := Publish(ctx, tx, ids, topic); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		changed map[string]int64
		err     error
		after   time.Duration
	}
	wait := func(machineID string, known map[string]int64, maxWait time.Duration) <-chan result {
		out := make(chan result, 1)
		// Creating the machines published their inventory; the host in this test has seen that already.
		seen := map[string]int64{}
		for _, topic := range topics { // a host waits on the topics it lists; this one lists them all
			seen[topic] = 0
		}
		for topic, generation := range known {
			seen[topic] = generation
		}
		if _, ok := known[TopicInventory]; !ok {
			var inventory int64
			if err := database.SQL().QueryRowContext(ctx, `SELECT (coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='inventory'),0)+coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=(SELECT user_id FROM paperboat.user_machines WHERE id=$1) AND topic='inventory'),0))`, machineID).Scan(&inventory); err != nil {
				t.Error(err)
			}
			seen[TopicInventory] = inventory
		}
		started := time.Now()
		go func() {
			changed, err := hub.Wait(ctx, machineID, seen, maxWait)
			out <- result{changed, err, time.Since(started)}
		}()
		return out
	}

	// Nothing has happened: the wait answers "no change" at its deadline.
	if r := <-wait(machine, map[string]int64{}, 700*time.Millisecond); r.err != nil || len(r.changed) != 0 || r.after < 600*time.Millisecond {
		t.Fatalf("idle wait = %+v", r)
	}
	// A publish wakes a waiting host in well under a second, and only for that machine and topic set.
	waiting := wait(machine, map[string]int64{}, 20*time.Second)
	bystander := wait(other, map[string]int64{}, 1500*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	publish([]string{machine}, TopicPreviews)
	r := <-waiting
	if r.err != nil || r.changed[TopicPreviews] != 1 || len(r.changed) != 1 || r.after > 2*time.Second {
		t.Fatalf("woken wait = %+v", r)
	}
	if b := <-bystander; b.err != nil || len(b.changed) != 0 {
		t.Fatalf("another machine was told: %+v", b)
	}
	// A topic the host does not wait on never wakes it.
	var inventoryNow int64
	if err := database.SQL().QueryRowContext(ctx, `SELECT (coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='inventory'),0)+coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=(SELECT user_id FROM paperboat.user_machines WHERE id=$1) AND topic='inventory'),0))`, other).Scan(&inventoryNow); err != nil {
		t.Fatal(err)
	}
	only := make(chan result, 1)
	go func() {
		changed, err := hub.Wait(ctx, other, map[string]int64{TopicInventory: inventoryNow}, 800*time.Millisecond)
		only <- result{changed: changed, err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	publish([]string{other}, TopicMachineServices)
	if r := <-only; r.err != nil || len(r.changed) != 0 {
		t.Fatalf("a topic the host does not wait on woke it: %+v", r)
	}
	// A host that was away compares what it knew: an older generation is answered at once, a current one waits.
	publish([]string{machine}, TopicPreviews)
	publish([]string{machine}, TopicMachineAccess)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 1}, 5*time.Second); r.err != nil || r.changed[TopicPreviews] != 2 || r.changed[TopicMachineAccess] != 1 || r.after > time.Second {
		t.Fatalf("catch-up wait = %+v", r)
	}
	if r := <-wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("up-to-date wait = %+v", r)
	}
	// A rolled-back write publishes nothing.
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, tx, []string{machine}, TopicConfig); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("rolled-back publish was seen: %+v", r)
	}
	// Writes that already exist in the product publish on their own (triggers): a preview for the
	// machine and an access session for it.
	waiting = wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 20*time.Second)
	time.Sleep(200 * time.Millisecond)
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`, "preview_"+stamp, "https://p-"+stamp+".preview.example.test", account, machine)
	if r := <-waiting; r.err != nil || r.changed[TopicPreviews] != 3 {
		t.Fatalf("a new preview did not wake its machine: %+v", r)
	}
	exec(`UPDATE paperboat.preview_leases SET lease_deadline=now()+interval '2 hours',last_renewed_at=now() WHERE id=$1`, "preview_"+stamp)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 3, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("a lease renewal woke the host: %+v", r)
	}
	exec(`UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, "preview_"+stamp)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 3, TopicMachineAccess: 1}, 5*time.Second); r.err != nil || r.changed[TopicPreviews] != 4 {
		t.Fatalf("a stopped preview did not wake its machine: %+v", r)
	}
	for _, bad := range []string{"unknown_topic"} {
		if _, err := hub.Wait(ctx, machine, map[string]int64{bad: 0}, time.Second); err == nil {
			t.Fatalf("unknown topic %q accepted", bad)
		}
	}
	// F21-09: on shutdown a held wait answers "no change" at once, and so does every later wait.
	held := wait(machine, map[string]int64{TopicPreviews: 4, TopicMachineAccess: 1}, 20*time.Second)
	time.Sleep(200 * time.Millisecond)
	hub.Drain()
	if r := <-held; r.err != nil || len(r.changed) != 0 || r.after > 2*time.Second {
		t.Fatalf("held wait on drain = %+v", r)
	}
	if r := <-wait(machine, map[string]int64{TopicPreviews: 4, TopicMachineAccess: 1}, 20*time.Second); r.err != nil || len(r.changed) != 0 || r.after > time.Second {
		t.Fatalf("wait after drain = %+v", r)
	}
}

func TestWritesThatChangeWhatAHostCachesPublishTheirTopicsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	account, stranger := "topics_account_"+stamp, "topics_stranger_"+stamp
	laptop, server, outsider := "topics_laptop_"+stamp, "topics_server_"+stamp, "topics_outsider_"+stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	generation := func(machine, topic string) int64 {
		t.Helper()
		var value int64
		err := database.SQL().QueryRowContext(ctx, `SELECT (coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2::text),0)+coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=(SELECT user_id FROM paperboat.user_machines WHERE id=$1) AND topic=$2::text),0))`, machine, topic).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	// moved runs write and reports which of the machines saw the topic move.
	moved := func(topic string, machines []string, write func()) map[string]bool {
		t.Helper()
		before := map[string]int64{}
		for _, machine := range machines {
			before[machine] = generation(machine, topic)
		}
		write()
		result := map[string]bool{}
		for _, machine := range machines {
			result[machine] = generation(machine, topic) > before[machine]
		}
		return result
	}
	expect := func(name string, got map[string]bool, want map[string]bool) {
		t.Helper()
		for machine, w := range want {
			if got[machine] != w {
				t.Errorf("%s: machine %s moved=%v, want %v", name, machine[:len(machine)-len(stamp)-1], got[machine], w)
			}
		}
	}
	for _, id := range []string{account, stranger} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test")
	}
	t.Cleanup(func() {
		for _, id := range []string{account, stranger} {
			_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, id)
		}
	})
	addMachine := func(id, owner, alias string) {
		exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,last_seen_at) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',now())`, id, owner, "env_"+id, alias)
	}
	addMachine(laptop, account, "laptop")
	addMachine(outsider, stranger, "outsider")
	all := []string{laptop, server, outsider}

	// inventory: a new machine tells the account's other machines, and nobody else.
	expect("new machine", moved(TopicInventory, []string{laptop, outsider}, func() { addMachine(server, account, "server") }), map[string]bool{laptop: true, outsider: false})
	expect("rename", moved(TopicInventory, all, func() { exec(`UPDATE paperboat.user_machines SET alias='renamed' WHERE id=$1`, server) }), map[string]bool{laptop: true, server: true, outsider: false})
	// Presence is not inventory: a flip used to wake every daemon of the account to re-list its machines.
	expect("presence", moved(TopicInventory, all, func() { exec(`UPDATE paperboat.user_machines SET online=NOT online WHERE id=$1`, server) }), map[string]bool{laptop: false, server: false, outsider: false})
	expect("offline", moved(TopicInventory, all, func() { exec(`UPDATE paperboat.user_machines SET state='offline' WHERE id=$1`, server) }), map[string]bool{laptop: false, server: false, outsider: false})
	exec(`UPDATE paperboat.user_machines SET state='online' WHERE id=$1`, server)
	expect("heartbeat only", moved(TopicInventory, all, func() { exec(`UPDATE paperboat.user_machines SET last_seen_at=now() WHERE id=$1`, server) }), map[string]bool{laptop: false, server: false})
	services := func(list string) {
		exec(`INSERT INTO paperboat.machine_service_snapshots(machine_id,installation_generation,boot_id,started_at,generation,services,expires_at,updated_at) VALUES($1,1,'boot-012345678901',now(),1,$2::jsonb,now()+interval '1 hour',now())
 ON CONFLICT(machine_id) DO UPDATE SET services=EXCLUDED.services,expires_at=EXCLUDED.expires_at,updated_at=now()`, server, list)
	}
	// machine_services: a machine's published ports tell the machines that can see it; the machine list
	// carries no services, so inventory does not move.
	expect("services appear", moved(TopicMachineServices, all, func() { services(`[{"port":3000,"address_family":"ipv4"}]`) }), map[string]bool{laptop: true, outsider: false})
	expect("services unchanged", moved(TopicMachineServices, all, func() { services(`[{"port":3000,"address_family":"ipv4"}]`) }), map[string]bool{laptop: false, server: false})
	expect("services change", moved(TopicMachineServices, all, func() { services(`[]`) }), map[string]bool{laptop: true, outsider: false})
	expect("services leave inventory", moved(TopicInventory, all, func() { services(`[{"port":8080,"address_family":"ipv4"}]`) }), map[string]bool{laptop: false, server: false, outsider: false})

	// previews: every machine that can see the owner machine learns of a preview, not only the owner.
	expect("preview", moved(TopicPreviews, all, func() {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`, "preview_"+stamp, "https://p-"+stamp+".preview.example.test", account, server)
	}), map[string]bool{laptop: true, server: true, outsider: false})

	// peer_network: new keys or a revocation reach the account's machines.
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
 VALUES(decode(repeat('02',32),'hex'),$1,$2,'machine',1,1,decode(repeat('aa',200),'hex'),decode(repeat('bb',32),'hex'),now(),now()+interval '1 day','key_01',decode(repeat('cc',32),'hex'))`, account, server)
	expect("peer identity", moved(TopicPeerNetwork, all, func() {
		exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,'machine',$2,1,1,1,decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),'100.64.0.9',now(),now())`, account, server)
	}), map[string]bool{laptop: true, server: true, outsider: false})
	expect("peer key rotation", moved(TopicPeerNetwork, all, func() {
		exec(`UPDATE paperboat.peer_network_identities SET wireguard_public_key=decode(repeat('03',32),'hex'),key_generation=2 WHERE endpoint_id=$1`, server)
	}), map[string]bool{laptop: true, outsider: false})

	// config: only the machine whose assignment changed.
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, "env_"+laptop, account)
	expect("config assignment", moved(TopicConfig, all, func() {
		exec(`INSERT INTO paperboat.control_config_assignments(id,environment_id,machine_id) VALUES($1,$2,$3)`, "assignment_"+stamp, "env_"+laptop, laptop)
	}), map[string]bool{laptop: true, server: false, outsider: false})
	expect("config version", moved(TopicConfig, all, func() {
		exec(`UPDATE paperboat.control_config_assignments SET version=version+1 WHERE id=$1`, "assignment_"+stamp)
	}), map[string]bool{laptop: true, server: false})

	// tunnels: the machine that created the tunnel.
	tunnel := "tunnel_" + stamp
	endpointID := testutil.EndpointID("topics:" + stamp)
	exec(`INSERT INTO paperboat.tunnels (id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code) VALUES ($1,$2,$3,'active','public',1,$4,$5,$6,$2,'pending')`, tunnel, account, "t-"+stamp, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".tunnels.example.test", laptop)
	expect("tunnel config", moved(TopicTunnels, all, func() {
		exec(`INSERT INTO paperboat.tunnel_config_generations (tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) VALUES ($1,1,$2,'{}','pending',$3,now()+interval '1 hour')`, tunnel, make([]byte, 32), account)
	}), map[string]bool{server: false, laptop: true, outsider: false})
	expect("tunnel deleted", moved(TopicTunnels, all, func() {
		exec(`UPDATE paperboat.tunnels SET desired_state='deleted',deleted_at=now() WHERE id=$1`, tunnel)
	}), map[string]bool{server: false, laptop: true, outsider: false})

	// config: the availability policy an idle host asks for only on this topic (PR-03).
	expect("availability", moved(TopicConfig, all, func() {
		exec(`UPDATE paperboat.user_machines SET availability_desired_version=availability_desired_version+1 WHERE id=$1`, server)
	}), map[string]bool{server: true, laptop: false, outsider: false})
	expect("unchanged availability", moved(TopicConfig, all, func() {
		exec(`UPDATE paperboat.user_machines SET availability_mode=availability_mode WHERE id=$1`, server)
	}), map[string]bool{server: false})

	// machine_access: a managed SSH client key wakes every machine of its account (PR-03).
	session := "topics_cli_" + stamp
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,last_used_at) VALUES($1,$2,'paperboat','topics','desktop','linux',ARRAY['machines:connect'],'active',now()-interval '3 days',now(),now()-interval '3 days')`, session, account)
	fingerprint := make([]byte, 32)
	fingerprint[0] = 1
	publicKey := "ssh-ed25519 " + strings.Repeat("A", 68) + " topics"
	expect("ssh key added", moved(TopicMachineAccess, all, func() {
		exec(`INSERT INTO paperboat.managed_ssh_client_keys(fingerprint,user_id,cli_client_session_id,algorithm,public_key) VALUES($1,$2,$3,'ssh-ed25519',$4)`, fingerprint, account, session, publicKey)
	}), map[string]bool{laptop: true, server: true, outsider: false})
	expect("idle session used again", moved(TopicMachineAccess, all, func() {
		exec(`UPDATE paperboat.cli_client_sessions SET last_used_at=now() WHERE id=$1`, session)
	}), map[string]bool{laptop: true, server: true, outsider: false})
	expect("session in daily use", moved(TopicMachineAccess, all, func() {
		exec(`UPDATE paperboat.cli_client_sessions SET last_used_at=now() WHERE id=$1`, session)
	}), map[string]bool{laptop: false, server: false})
	expect("ssh key revoked", moved(TopicMachineAccess, all, func() {
		exec(`UPDATE paperboat.managed_ssh_client_keys SET state='revoked',revoked_at=now(),revocation_reason='client_revoked' WHERE cli_client_session_id=$1`, session)
	}), map[string]bool{laptop: true, server: true, outsider: false})
}
