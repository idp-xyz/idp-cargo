-- IDP-Cargo COM V0.9 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- C12, DOC OD-2 option A: a transport document holds a lock on its orders' parties and transport from issue
-- until it is cancelled or amended with other parties. While any lock is held, COM refuses to write a new
-- party or transport version; the holder releases its lock, once. COM never asks DOC: the lock is COM's own
-- fact, set and cleared through COM's api. Existing orders hold no lock.

CREATE TABLE cargo_com.order_party_lock (
  tenant_id text NOT NULL,
  order_id text NOT NULL,
  lock_no integer NOT NULL CHECK (lock_no > 0),
  holder_id text NOT NULL CHECK (length(btrim(holder_id)) BETWEEN 1 AND 128),
  locked_by text NOT NULL CHECK (length(btrim(locked_by)) > 0),
  locked_at timestamptz NOT NULL,
  entered_at timestamptz NOT NULL,
  released_by text CHECK (length(btrim(released_by)) > 0),
  released_at timestamptz,
  release_entered_at timestamptz,
  PRIMARY KEY (tenant_id, order_id, lock_no),
  FOREIGN KEY (tenant_id, order_id) REFERENCES cargo_com.customer_order (tenant_id, order_id),
  CONSTRAINT order_party_lock_release CHECK ((released_at IS NULL) = (released_by IS NULL) AND (released_at IS NULL) = (release_entered_at IS NULL)
    AND (released_at IS NULL OR released_at >= locked_at))
);
-- A holder holds at most one lock in force on an order.
CREATE UNIQUE INDEX order_party_lock_one_per_holder ON cargo_com.order_party_lock (tenant_id, order_id, holder_id) WHERE released_at IS NULL;

-- A lock is only released, once.
CREATE FUNCTION cargo_com.release_order_party_lock_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  releases text[] := ARRAY['released_by','released_at','release_entered_at'];
BEGIN
  IF TG_OP = 'DELETE' OR OLD.released_at IS NOT NULL OR NEW.released_at IS NULL OR to_jsonb(OLD) - releases IS DISTINCT FROM to_jsonb(NEW) - releases THEN
    RAISE EXCEPTION 'cargo_com: party lock %/%/% can only be released, once', OLD.tenant_id, OLD.order_id, OLD.lock_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER order_party_lock_release_only BEFORE UPDATE OR DELETE ON cargo_com.order_party_lock
  FOR EACH ROW EXECUTE FUNCTION cargo_com.release_order_party_lock_only();

-- At commit, a party or transport version is not written while a lock on the order is in force (C12).
CREATE FUNCTION cargo_com.check_order_party_unlocked() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM cargo_com.order_party_lock l WHERE l.tenant_id = NEW.tenant_id AND l.order_id = NEW.order_id AND l.released_at IS NULL) THEN
    RAISE EXCEPTION 'cargo_com: order %/%: its parties are locked by a transport document', NEW.tenant_id, NEW.order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_parties_locked';
  END IF;
  RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER order_party_unlocked AFTER INSERT ON cargo_com.order_party
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_party_unlocked();
CREATE CONSTRAINT TRIGGER order_transport_unlocked AFTER INSERT ON cargo_com.order_transport
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cargo_com.check_order_party_unlocked();

ALTER TABLE cargo_com.order_party_lock ENABLE ROW LEVEL SECURITY;
ALTER TABLE cargo_com.order_party_lock FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cargo_com.order_party_lock
  USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
