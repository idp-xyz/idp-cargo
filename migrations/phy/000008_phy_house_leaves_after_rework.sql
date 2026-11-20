-- IDP-Cargo PHY V0.6 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A house unloaded whole in the rework of a sealed container leaves its CLOSED consolidation (V2.86; the second of
-- the steps of "a house leaves a sealed container"). Until now the live membership entry of a CLOSED consolidation
-- could end only as SHIPMENT_CANCELLED (membership_entry_frozen, 000001). It may now also end as DETACHED or
-- TRANSFERRED, with the operator's reason (the columns and checks of the entry's end are unchanged), when the house
-- has no live packs in the consolidation and was unloaded by a rework of it: a reversal of one of its lines in an
-- execution of the consolidation carries a rework number (reversal_line.rework_no, 000007). A house that was never
-- loaded has no such reversal and stays: that is a shut-out before stuffing, decided while the consolidation is open.
-- Whether the container it was unloaded from is still in REWORK or sealed again does not matter, and no other house
-- of a CLOSED consolidation moves.
--
-- The entry that ends records which rework unloaded the house: end_rework_execution_id and end_rework_no, both set
-- together and only on an entry that ended as DETACHED or TRANSFERRED in a CLOSED consolidation, and both NULL on
-- every other entry, including one that ends while the consolidation is OPEN. The database checks the pair against a
-- reversal of the house, in the consolidation, that carries that rework number in that execution. A reversal only
-- ever names a rework of its own execution (cargo_phy.set_reversal_rework sets it from the execution's open rework),
-- so the pair needs no foreign key. Which of several reworks that unloaded the house the entry names (the latest, by
-- the time PHY entered the reversals) is the domain's choice; the database accepts any rework that unloaded the house
-- there.
--
-- What the database does not keep is what the domain keeps: that the reason is one an operator gives (the checks of
-- the entry's end allow SHIPMENT_CANCELLED only with the end kind of that name, as before, and OTHER only with a
-- text), that a transfer's target is OPEN and takes the house (the new entry's deferred check,
-- membership_entry_consolidation_open of 000001, refuses a target that is not OPEN, and nothing here checks its
-- mode), that the rework named is the latest, that the entry is not dated before the house was unloaded, and that the
-- house was unloaded whole (reversal_line_rework_house of 000007 checks that when it is unloaded). Live packs are
-- counted by cargo_phy.live_packs_of_house; the deferred membership_entry_loaded of 000002 still refuses at commit an
-- entry that ended with live packs, and the entry's own rule below refuses it earlier, under its own name.
--
-- The owner decided on 2026-11-20 that a house unloaded by a rework of the consolidation, loaded again into another
-- container of it and unloaded there by an ordinary reversal, may leave, naming that rework: "after the unloading no
-- live load exists" means no live packs. Objects added: the trigger membership_entry_insert (an entry is inserted
-- live, by name membership_entry_inserted_live; CREATE TRIGGER takes SHARE ROW EXCLUSIVE on membership_entry).
--
-- Objects of earlier migrations replaced here, each with every rule of its latest definition (file of that
-- definition):
--   cargo_phy.check_membership_entry()   000001_phy_consolidation_membership (the only one)
--
-- Not replaced, and checked: check_membership_entry_loaded (000002); check_live_entry_open (000001), which still
-- refuses a live entry, a transfer's new one included, in a consolidation that is not OPEN;
-- check_consolidation_current (000001), which still refuses a cancelled consolidation that keeps a live house.
--
-- Locks: ALTER TABLE takes ACCESS EXCLUSIVE on cargo_phy.membership_entry for its own duration. The two new columns
-- have no default, so the table is not rewritten; the new CHECK constraint scans the table once to validate it (every
-- new value is NULL). CREATE OR REPLACE FUNCTION locks no table. No existing row changes.

ALTER TABLE cargo_phy.membership_entry
  ADD COLUMN end_rework_execution_id text,
  ADD COLUMN end_rework_no integer,
  ADD CONSTRAINT membership_entry_rework CHECK ((end_rework_execution_id IS NULL) = (end_rework_no IS NULL)
    AND (end_rework_no IS NULL OR end_kind IN ('DETACHED','TRANSFERRED')));

-- An entry is only ended, once. While its consolidation is CLOSED (the house cut-off), only a cancelled shipment's
-- entry may end, and the entry of a house that was unloaded in a rework of the consolidation and has no live packs
-- left in it, as DETACHED or TRANSFERRED, naming that rework (V2.86). An entry that ends while the consolidation is
-- OPEN names no rework. The locks are taken in the order of every command (application/ports.go: consolidations, load
-- executions, membership ledgers, cargo lots): the consolidation FOR SHARE first, then the house's ledger FOR NO KEY
-- UPDATE, and only then is the house's load read. A command has locked both before it updates the entry. A writer
-- outside the commands locks the entry's row by its UPDATE before this trigger runs, and a command holds the ledger and
-- waits for that row, so a trigger that waited for the ledger would deadlock with it (a probe on the first version of
-- this trigger did, 3 to 4 runs in 25). The ledger is therefore asked for without waiting: a writer that meets a
-- command, or a load of the house, is refused at once with lock_not_available; one that locks the consolidation and the
-- ledger itself first, as the commands do, never meets it. The reversals that name a rework never change.
CREATE OR REPLACE FUNCTION cargo_phy.check_membership_entry() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ends text[] := ARRAY['end_kind','end_reason','end_text','ended_by','ended_at','end_entered_at','successor_no',
    'end_rework_execution_id','end_rework_no'];
  s text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: membership entries are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
    RAISE EXCEPTION 'cargo_phy: membership entry %/%/% can only be ended, once', OLD.tenant_id, OLD.shipment_id, OLD.entry_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT state INTO s FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
  PERFORM 1 FROM cargo_phy.consol_membership WHERE tenant_id = NEW.tenant_id AND shipment_id = NEW.shipment_id AND stage = NEW.stage FOR NO KEY UPDATE NOWAIT;
  IF s IS DISTINCT FROM 'OPEN' AND NOT (s = 'CLOSED' AND NEW.end_kind IN ('SHIPMENT_CANCELLED','DETACHED','TRANSFERRED')) THEN
    RAISE EXCEPTION 'cargo_phy: consolidation %/% is %; its houses are frozen', NEW.tenant_id, NEW.consolidation_id, s
      USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_frozen';
  END IF;
  IF s = 'OPEN' AND NEW.end_rework_no IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_phy: entry %/%/% ends in the open consolidation % and names no rework', NEW.tenant_id, NEW.shipment_id, NEW.entry_no,
      NEW.consolidation_id USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_rework_link';
  END IF;
  IF s = 'CLOSED' AND NEW.end_kind IN ('DETACHED','TRANSFERRED') THEN
    IF NOT EXISTS (SELECT 1 FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line l
         ON l.tenant_id = r.tenant_id AND l.execution_id = r.execution_id AND l.line_no = r.line_no
         WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.end_rework_execution_id AND r.rework_no = NEW.end_rework_no
           AND l.consolidation_id = NEW.consolidation_id AND l.shipment_id = NEW.shipment_id) THEN
      RAISE EXCEPTION 'cargo_phy: house %/% was not unloaded in a rework of consolidation %/%; its entry is frozen', NEW.tenant_id, NEW.shipment_id,
        NEW.tenant_id, NEW.consolidation_id USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_rework_link';
    END IF;
    IF cargo_phy.live_packs_of_house(NEW.tenant_id, NEW.consolidation_id, NEW.shipment_id) > 0 THEN
      RAISE EXCEPTION 'cargo_phy: house %/% still has packs loaded in the closed consolidation %', NEW.tenant_id, NEW.shipment_id, NEW.consolidation_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_rework_loaded';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;

-- An entry is inserted live, with no rework named: nothing writes an entry that is already ended (a house command
-- ends an entry that exists and starts the next one live), and the rules of its end are those of
-- check_membership_entry, which fires on UPDATE only. Before this, a raw insert of an ended entry in a CLOSED
-- consolidation was accepted with no rework named, or one that never existed.
CREATE FUNCTION cargo_phy.check_membership_entry_insert() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.end_kind IS NOT NULL OR NEW.ended_at IS NOT NULL OR NEW.end_rework_execution_id IS NOT NULL OR NEW.end_rework_no IS NOT NULL THEN
    RAISE EXCEPTION 'cargo_phy: membership entry %/%/% is inserted live; an entry is ended by an update', NEW.tenant_id, NEW.shipment_id, NEW.entry_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_inserted_live';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER membership_entry_insert BEFORE INSERT ON cargo_phy.membership_entry
  FOR EACH ROW EXECUTE FUNCTION cargo_phy.check_membership_entry_insert();
