package application

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/batch"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 数据库为原生 PostgreSQL；此替身只模拟原生身份树与确定性交错点。
type repairNative struct {
	*mutationStore
	parents      map[string]string
	pauseID      string
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
	unknownMove  bool
	unknownMkdir bool
}

func repairFixture(t *testing.T) (*Service, Actor, *repairNative) {
	s, a, store := mutationFixture(t)
	n := &repairNative{mutationStore: store, parents: map[string]string{}}
	s.Original = n
	return s, a, n
}

func (n *repairNative) CreateDirectory(ctx context.Context, r ports.DirectoryRequest) (ports.OriginalMetadata, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, v := range n.objects {
		if n.parents[id] == r.Parent.ResourceID && v.Name == r.Name {
			return ports.OriginalMetadata{}, ports.ErrConflict
		}
	}
	v := ports.OriginalMetadata{ResourceRef: ports.ResourceRef{Instance: r.Parent.Instance, SpaceID: r.Parent.SpaceID, ResourceID: id()}, Name: r.Name, ETag: "created", IsDirectory: true}
	n.objects[v.ResourceID] = v
	n.parents[v.ResourceID] = r.Parent.ResourceID
	n.mkdirs++
	if n.unknownMkdir {
		n.unknownMkdir = false
		return ports.OriginalMetadata{}, ports.ErrOutcomeUnknown
	}
	return v, nil
}

func (n *repairNative) Move(ctx context.Context, r ports.MoveRequest) (ports.OriginalMetadata, error) {
	if r.Source.ResourceID == n.pauseID {
		n.once.Do(func() { close(n.entered) })
		select {
		case <-n.release:
		case <-ctx.Done():
			return ports.OriginalMetadata{}, ctx.Err()
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.objects[r.Source.ResourceID]
	if !ok {
		return v, ports.ErrNotFound
	}
	if v.ETag != r.ExpectedETag {
		return v, ports.ErrConflict
	}
	for id, other := range n.objects {
		if id != v.ResourceID && n.parents[id] == r.DestinationParent.ResourceID && other.Name == r.Name {
			return ports.OriginalMetadata{}, ports.ErrConflict
		}
	}
	n.moves++
	v.Name, v.ETag = r.Name, fmt.Sprint(n.moves)
	n.objects[v.ResourceID], n.parents[v.ResourceID] = v, r.DestinationParent.ResourceID
	if n.unknownMove && r.Source.ResourceID == n.pauseID {
		n.unknownMove = false
		return ports.OriginalMetadata{}, ports.ErrOutcomeUnknown
	}
	return v, nil
}

func (n *repairNative) LookupChild(ctx context.Context, parent ports.ResourceRef, name string, cred ports.Credential) (ports.OriginalMetadata, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, v := range n.objects {
		if v.SpaceID == parent.SpaceID && n.parents[id] == parent.ResourceID && v.Name == name {
			return v, nil
		}
	}
	return ports.OriginalMetadata{}, ports.ErrNotFound
}

func repairMkdir(t *testing.T, s *Service, a Actor, name string, parent Object) Object {
	t.Helper()
	in := Object{"name": name, "idempotency_key": id()}
	if parent != nil {
		in["parent_id"], in["expected_parent_revision"] = str(parent, "id"), num(parent, "metadata_revision")
	}
	r, err := s.Mutate(context.Background(), a, "mkdir", in)
	if err != nil {
		t.Fatal(err)
	}
	return r.(map[string]any)["file"].(map[string]any)
}

func repairDue(t *testing.T, s *Service) {
	t.Helper()
	_, err := s.DB.Exec(context.Background(), `UPDATE file_operations SET lease_until=NULL,retry_at=now(),updated_at=now()-interval '6 minutes' WHERE state NOT IN('committed','failed_confirmed')`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRepairLifecycleInboundHolds(t *testing.T) {
	for _, target := range []string{"parent", "ancestor"} {
		for _, kind := range []string{"rename", "move", "delete"} {
			t.Run(target+"-"+kind, func(t *testing.T) {
				s, a, n := repairFixture(t)
				ancestor := repairMkdir(t, s, a, "ancestor", nil)
				parent := repairMkdir(t, s, a, "target", ancestor)
				other := repairMkdir(t, s, a, "other", nil)
				source := repairMkdir(t, s, a, "source", nil)
				n.pauseID, n.entered, n.release, n.unknownMove = str(source, "native_resource_id"), make(chan struct{}), make(chan struct{}), true
				// 公共响应隐藏 native ID，从内部投影读取稳定身份。
				if err := s.DB.QueryRow(context.Background(), `SELECT native_resource_id FROM files WHERE id=$1`, str(source, "id")).Scan(&n.pauseID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := s.Mutate(ctx, a, "move", Object{"file_id": str(source, "id"), "target_parent_id": str(parent, "id"), "expected_metadata_revision": num(source, "metadata_revision"), "idempotency_key": "inbound"})
					done <- err
				}()
				select {
				case <-n.entered:
				case <-ctx.Done():
					t.Fatal("MOVE未到交错点")
				}
				f := parent
				if target == "ancestor" {
					f = ancestor
				}
				in := Object{"file_id": str(f, "id"), "name": "changed", "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "competitor"}
				if kind == "move" {
					in["target_parent_id"] = str(other, "id")
				}
				_, err := s.Mutate(ctx, a, kind, in)
				close(n.release)
				if domain.ErrorCode(normalizeError(err)) != domain.OperationPending {
					t.Fatal("竞争方未被占用图拒绝", err)
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				restarted := &Service{DB: s.DB, Config: s.Config, Original: s.Original, Identity: s.Identity}
				repairDue(t, s)
				if err = restarted.ReconcileMutations(ctx); err != nil {
					t.Fatal(err)
				}
				if err = restarted.ReconcileMutations(ctx); err != nil {
					t.Fatal(err)
				}
				var p, path, state, native string
				err = s.DB.QueryRow(ctx, `SELECT f.parent_id::text,f.path,o.state,f.native_resource_id FROM files f JOIN file_operations o ON o.id=(SELECT id FROM file_operations WHERE idempotency_key='inbound') WHERE f.id=$1`, str(source, "id")).Scan(&p, &path, &state, &native)
				if err != nil || p != str(parent, "id") || path != "/ancestor/target/source" || state != "committed" {
					t.Fatal(p, path, state, err)
				}
				n.mu.Lock()
				nativeParent := n.parents[native]
				moves := n.moves
				n.mu.Unlock()
				var expected string
				_ = s.DB.QueryRow(ctx, `SELECT native_resource_id FROM files WHERE id=$1`, p).Scan(&expected)
				if nativeParent != expected || moves != 1 {
					t.Fatal("原生树不一致或重复MOVE", nativeParent, expected, moves)
				}
			})
		}
	}
}

func TestRepairLifecycleHistoricalTargetConvergence(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint("deleted=", deleted), func(t *testing.T) {
			s, a, n := repairFixture(t)
			parent := repairMkdir(t, s, a, "target", nil)
			source := repairMkdir(t, s, a, "source", nil)
			_ = s.DB.QueryRow(context.Background(), `SELECT native_resource_id FROM files WHERE id=$1`, str(source, "id")).Scan(&n.pauseID)
			n.entered, n.release, n.unknownMove = make(chan struct{}), make(chan struct{}), true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, e := s.Mutate(ctx, a, "move", Object{"file_id": str(source, "id"), "target_parent_id": str(parent, "id"), "expected_metadata_revision": num(source, "metadata_revision"), "idempotency_key": "historical"})
				done <- e
			}()
			select {
			case <-n.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// 受控 SQL 模拟升级前已发生的目标变化；不代表合法业务能绕过 holds。
			newPath, lifecycle := "/changed", "active"
			if deleted {
				newPath, lifecycle = "/.retained/old-target", "trashed"
			}
			_, e := s.DB.Exec(ctx, `UPDATE files SET path=$2,name='changed',lifecycle=$3,metadata_revision=metadata_revision+1 WHERE id=$1`, str(parent, "id"), newPath, lifecycle)
			if e != nil {
				close(n.release)
				t.Fatal(e)
			}
			close(n.release)
			if e = <-done; e != nil {
				t.Fatal(e)
			}
			// 模拟原生已成功且 DB 在最终提交之前退出。
			op, e := one(ctx, s.DB, `SELECT to_jsonb(o) FROM file_operations o WHERE idempotency_key='historical'`)
			if e != nil {
				t.Fatal(e)
			}
			meta, _ := n.Stat(ctx, ports.ResourceRef{ResourceID: n.pauseID}, ports.Credential{})
			_, e = s.DB.Exec(ctx, `UPDATE file_operations SET state='remote_confirmed',remote_result=$2::jsonb WHERE id=$1`, str(op, "id"), js(Object{"metadata": meta}))
			if e != nil {
				t.Fatal(e)
			}
			repairDue(t, s)
			restarted := &Service{DB: s.DB, Config: s.Config, Original: s.Original, Identity: s.Identity}
			if deleted {
				n.unknownMove = true
				if e = restarted.ReconcileMutations(ctx); e != ports.ErrOutcomeUnknown {
					t.Fatal("补偿响应丢失未保留恢复责任", e)
				}
				repairDue(t, s)
			}
			if e = restarted.ReconcileMutations(ctx); e != nil {
				t.Fatal(e)
			}
			if e = restarted.ReconcileMutations(ctx); e != nil {
				t.Fatal(e)
			}
			var path, state string
			e = s.DB.QueryRow(ctx, `SELECT f.path,o.state FROM files f JOIN file_operations o ON o.file_id=f.id WHERE o.id=$1`, str(op, "id")).Scan(&path, &state)
			want := "/changed/source"
			if deleted {
				want = "/source"
			}
			if e != nil || state != "committed" || path != want {
				t.Fatal(path, state, e)
			}
			n.mu.Lock()
			nativeParent := n.parents[n.pauseID]
			n.mu.Unlock()
			if deleted && nativeParent != "root" {
				t.Fatal("补偿未回到原位置", nativeParent)
			}
		})
	}
}

func repairPool(t *testing.T, s *Service, n int32) {
	t.Helper()
	cfg := s.DB.Config()
	cfg.MaxConns = n
	pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	s.DB = pool
}

func TestRepairBatchSingleConnection(t *testing.T) {
	s, a, n := repairFixture(t)
	repairPool(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := Object{"idempotency_key": "single", "items": []Object{{"item_id": "a", "command": "mkdir", "input": Object{"name": "batch-file"}}}}
	for i := 0; i < 2; i++ {
		r, e := s.Batch(ctx, a, in)
		if e != nil || str(r.(map[string]any), "state") != "completed" {
			t.Fatal(r, e)
		}
	}
	n.mu.Lock()
	calls := n.mkdirs
	n.mu.Unlock()
	if calls != 1 {
		t.Fatal("重放重复副作用", calls)
	}
}

func TestRepairBatchConcurrentBatchesExhaustPool(t *testing.T) {
	s, a, _ := repairFixture(t)
	repairPool(t, s, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, e := batch.New(s.deps()).Batch(ctx, a, Object{"idempotency_key": fmt.Sprint(i), "items": []Object{{"item_id": "a", "command": "mkdir", "input": Object{"name": fmt.Sprint("batch-", i)}}}}, func(ctx context.Context, a Actor, name string, in Object) (any, error) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return s.Mutate(ctx, a, name, in)
			})
			done <- e
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("批次未推进到项目", ctx.Err())
		}
	}
	var one int
	if e := s.DB.QueryRow(ctx, `SELECT 1`).Scan(&one); e != nil {
		close(release)
		t.Fatal("普通读取被批次占满池", e)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
}

func TestRepairBatchConcurrentReplayAndFence(t *testing.T) {
	s, a, n := repairFixture(t)
	repairPool(t, s, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := Object{"idempotency_key": "same-batch", "items": []Object{{"item_id": "a", "command": "mkdir", "input": Object{"name": "same-item"}}}}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := batch.New(s.deps()).Batch(ctx, a, in, func(ctx context.Context, a Actor, name string, in Object) (any, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return s.Mutate(ctx, a, name, in)
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	r, err := s.Batch(ctx, a, in)
	if err != nil || str(r.(map[string]any), "state") != "running" {
		close(release)
		t.Fatal(r, err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	calls := n.mkdirs
	n.mu.Unlock()
	if calls != 1 {
		t.Fatal("同批并发重放产生重复副作用", calls)
	}
	in["idempotency_key"] = "fenced"
	_, err = batch.New(s.deps()).Batch(ctx, a, in, func(ctx context.Context, a Actor, name string, in Object) (any, error) {
		_, e := s.DB.Exec(ctx, `UPDATE batch_operations SET lease_token=lease_token+1 WHERE idempotency_key='fenced'`)
		return Object{"state": "committed"}, e
	})
	if domain.ErrorCode(err) != domain.LeaseLost {
		t.Fatal("过期批次所有者未被fence", err)
	}
	var status string
	if err = s.DB.QueryRow(ctx, `SELECT i.status FROM batch_items i JOIN batch_operations b ON b.id=i.batch_id WHERE b.idempotency_key='fenced'`).Scan(&status); err != nil || status != "running" {
		t.Fatal("旧所有者写回结果", status, err)
	}
}

func TestRepairBatchCancellationAndLeaseTakeover(t *testing.T) {
	s, a, n := repairFixture(t)
	repairPool(t, s, 1)
	in := Object{"idempotency_key": "cancel", "items": []Object{{"item_id": "a", "command": "mkdir", "input": Object{"name": "once"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	_, e := batch.New(s.deps()).Batch(ctx, a, in, func(ctx context.Context, a Actor, name string, in Object) (any, error) {
		r, e := s.Mutate(ctx, a, name, in)
		cancel()
		return r, e
	})
	if e == nil {
		t.Fatal("取消应保留运行中项目供恢复")
	}
	// 模拟崩溃遗留租约；未过期时同批只返回快照，不执行。
	_, e = s.DB.Exec(context.Background(), `UPDATE batch_operations SET lease_owner='crashed',lease_until=now()+interval '1 minute' WHERE idempotency_key='cancel'`)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Batch(context.Background(), a, in)
	if e != nil || str(r.(map[string]any), "state") != "running" {
		t.Fatal(r, e)
	}
	_, e = s.DB.Exec(context.Background(), `UPDATE batch_operations SET lease_until=now()-interval '1 second' WHERE idempotency_key='cancel'`)
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Batch(context.Background(), a, in)
	if e != nil || str(r.(map[string]any), "state") != "completed" {
		t.Fatal(r, e)
	}
	n.mu.Lock()
	calls := n.mkdirs
	n.mu.Unlock()
	if calls != 1 {
		t.Fatal("接管重复副作用", calls)
	}
}

func TestRepairRetentionCrashAndLostResponse(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint("before=", before), func(t *testing.T) {
			s, a, n := repairFixture(t)
			f := repairMkdir(t, s, a, "victim", nil)
			ctx := context.Background()
			if before {
				_, e := s.DB.Exec(ctx, `INSERT INTO retention_roots(space_id,state,directory_name,native_parent_id,lease_until) SELECT id,'executing','.retained','root',now()-interval '1 second' FROM spaces`)
				if e != nil {
					t.Fatal(e)
				}
			} else {
				n.unknownMkdir = true
				_, e := s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "first"})
				if domain.ErrorCode(e) != domain.OperationPending {
					t.Fatal(e)
				}
				_, e = s.DB.Exec(ctx, `UPDATE retention_roots SET retry_at=now()`)
				if e != nil {
					t.Fatal(e)
				}
			}
			restarted := &Service{DB: s.DB, Config: s.Config, Original: s.Original, Identity: s.Identity}
			for i := 0; i < 2; i++ {
				if e := restarted.ReconcileRetentionRoots(ctx); e != nil {
					t.Fatal(e)
				}
			}
			for _, name := range []string{"victim", "second"} {
				if name == "second" {
					f = repairMkdir(t, s, a, name, nil)
				}
				r, e := restarted.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": name})
				if e != nil || str(r.(map[string]any), "state") != "committed" {
					t.Fatal(r, e)
				}
			}
			n.mu.Lock()
			count := 0
			for _, v := range n.objects {
				if v.Name == ".retained" {
					count++
				}
			}
			n.mu.Unlock()
			if count != 1 {
				t.Fatal("保留根重复", count)
			}
		})
	}
}

func TestRepairRetentionConcurrentFirstDeletes(t *testing.T) {
	s, a, n := repairFixture(t)
	files := []Object{repairMkdir(t, s, a, "first", nil), repairMkdir(t, s, a, "second", nil)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, f := range files {
		wg.Add(1)
		go func(i int, f Object) {
			defer wg.Done()
			_, e := s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": fmt.Sprint("delete", i)})
			errs <- e
		}(i, f)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil && domain.ErrorCode(e) != domain.OperationPending {
			t.Fatal(e)
		}
	}
	for i, f := range files {
		r, e := s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": fmt.Sprint("delete", i)})
		if e != nil || str(r.(map[string]any), "state") != "committed" {
			t.Fatal(r, e)
		}
	}
	n.mu.Lock()
	count := 0
	for _, v := range n.objects {
		if v.Name == ".retained" {
			count++
		}
	}
	n.mu.Unlock()
	if count != 1 {
		t.Fatal(count)
	}
}

func TestRepairLifecycleRestoreOnlyScope(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint("nested=", nested), func(t *testing.T) {
			s, a, _ := repairFixture(t)
			parent := repairMkdir(t, s, a, "target", nil)
			f := repairMkdir(t, s, a, "restore-me", nil)
			r, e := s.Mutate(context.Background(), a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "delete"})
			if e != nil {
				t.Fatal(e)
			}
			f = r.(map[string]any)["file"].(map[string]any)
			var trash string
			if e = s.DB.QueryRow(context.Background(), `SELECT id::text FROM trash_entries WHERE root_file_id=$1`, str(f, "id")).Scan(&trash); e != nil {
				t.Fatal(e)
			}
			a.Principal.Type = domain.ActorAgent
			a.Principal.Scopes = map[string]bool{domain.ScopeRestore: true}
			in := Object{"trash_id": trash, "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "restore"}
			if nested {
				in["target_parent_id"], in["expected_parent_revision"] = str(parent, "id"), num(parent, "metadata_revision")
			}
			without := a
			without.Principal.Scopes = map[string]bool{}
			if _, e = s.Mutate(context.Background(), without, "restore", in); domain.ErrorCode(e) != domain.InsufficientScope {
				t.Fatal(e)
			}
			if nested {
				in["expected_parent_revision"] = int64(999)
				if _, e = s.Mutate(context.Background(), a, "restore", in); domain.ErrorCode(e) != domain.VersionConflict {
					t.Fatal(e)
				}
				in["expected_parent_revision"] = num(parent, "metadata_revision")
				in["name"] = "target"
				in["target_parent_id"] = ""
				if _, e = s.Mutate(context.Background(), a, "restore", in); domain.ErrorCode(e) != domain.PathConflict {
					t.Fatal(e)
				}
				delete(in, "name")
				in["target_parent_id"] = str(parent, "id")
			}
			r, e = s.Mutate(context.Background(), a, "restore", in)
			if e != nil || str(r.(map[string]any), "state") != "committed" {
				t.Fatal(r, e)
			}
			if a.Principal.Scopes[domain.ScopeWrite] {
				t.Fatal("restore扩大权限")
			}
			if _, e = s.Mutate(context.Background(), a, "mkdir", Object{"name": "forbidden", "idempotency_key": "write"}); domain.ErrorCode(e) != domain.InsufficientScope {
				t.Fatal(e)
			}
		})
	}
}

func TestRepairLifecycleRestoreParentDenials(t *testing.T) {
	s, a, _ := repairFixture(t)
	ancestor := repairMkdir(t, s, a, "ancestor", nil)
	parent := repairMkdir(t, s, a, "parent", ancestor)
	a.Principal.Type = domain.ActorAgent
	a.Principal.Scopes = map[string]bool{domain.ScopeRestore: true}
	ctx := context.Background()
	other := a
	other.Principal.ID = id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','other');`, other.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO spaces(owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,'http://ocis.test','other-space','other-root')`, other.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.New(s.deps()).RestoreParent(ctx, other, str(parent, "id")); domain.ErrorCode(err) != domain.NotFound {
		t.Fatal("其他owner目标未拒绝", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, str(ancestor, "id")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.New(s.deps()).RestoreParent(ctx, a, str(parent, "id")); domain.ErrorCode(err) != domain.NotFound {
		t.Fatal("已删除祖先未拒绝", err)
	}
}

func TestRepairRetentionIdentityMismatchAndFence(t *testing.T) {
	s, a, n := repairFixture(t)
	f := repairMkdir(t, s, a, "victim", nil)
	ctx := context.Background()
	_, err := s.DB.Exec(ctx, `INSERT INTO retention_roots(space_id,state,directory_name,native_parent_id) SELECT id,'executing','.retained','root' FROM spaces`)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.objects["foreign"] = ports.OriginalMetadata{ResourceRef: ports.ResourceRef{SpaceID: "native-space", ResourceID: "foreign"}, Name: ".retained", IsDirectory: false}
	n.parents["foreign"] = "root"
	n.mu.Unlock()
	if err = s.ReconcileRetentionRoots(ctx); domain.ErrorCode(err) != domain.OperationPending {
		t.Fatal(err)
	}
	var state, diagnostic string
	if err = s.DB.QueryRow(ctx, `SELECT state,last_error FROM retention_roots`).Scan(&state, &diagnostic); err != nil || state != "outcome_unknown" || diagnostic != "NATIVE_IDENTITY_MISMATCH" {
		t.Fatal(state, diagnostic, err)
	}
	if _, err = s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "blocked"}); domain.ErrorCode(err) != domain.OperationPending {
		t.Fatal("不确定根错误释放门禁", err)
	}
	_, err = s.DB.Exec(ctx, `UPDATE retention_roots SET retry_at=now()`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.claimRetentionRoot(ctx, str(f, "space_id"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(ctx, `UPDATE retention_roots SET lease_token=lease_token+1`)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.objects["foreign"] = ports.OriginalMetadata{ResourceRef: ports.ResourceRef{SpaceID: "native-space", ResourceID: "foreign"}, Name: ".retained", IsDirectory: true}
	n.mu.Unlock()
	_, err = s.recoverRetentionRoot(ctx, r, ports.ResourceRef{SpaceID: "native-space", ResourceID: "root"}, ports.Credential{})
	if domain.ErrorCode(err) != domain.LeaseLost {
		t.Fatal("旧lease写入未拒绝", err)
	}
}

func TestRepairLifecycleMigrationReentrant(t *testing.T) {
	s, _, _ := repairFixture(t)
	sql, e := os.ReadFile("../../db/migrations/000010_lifecycle_occupancy.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(context.Background(), string(sql)); e != nil {
		t.Fatal(e)
	}
	var version string
	if e = s.DB.QueryRow(context.Background(), `SELECT version()`).Scan(&version); e != nil || !strings.Contains(version, "PostgreSQL") {
		t.Fatal(version, e)
	}
	t.Log(version)
}
