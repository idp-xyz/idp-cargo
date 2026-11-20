-- IDP-Cargo PHY V0.7 / PostgreSQL 16. Run only as the migrator role, never at service start.
-- A house with no live packs leaves its CLOSED sea consolidation whether or not a rework unloaded it (V2.87; step 2b of
-- the steps of "a house leaves a sealed container"). V2.86 (000008) let the live entry of a CLOSED consolidation end as
-- DETACHED or TRANSFERRED only naming a rework that had unloaded the house, so a house shut out before stuffing, or
-- taken out by ordinary reversals only, stayed attached to a consolidation that then could never reconcile. The owner
-- decided on 2026-11-20 that such a house leaves too, with the operator's reason.
--
-- Only the CLOSED case of the entry's end changes. As before, the house has no live packs in the consolidation
-- (membership_entry_rework_loaded). Then, by what the house's lines in the consolidation have in their reversals:
--   - if a reversal of one of them carries a rework number (reversal_line.rework_no, 000007), the end names one such
--     rework, as in 000008 (membership_entry_rework_link);
--   - if none does, the end names no rework, and the consolidation is of a sea kind (membership_entry_closed_sea_only,
--     new). A house of a CLOSED AIR_CONSOL with no rework stays frozen: AIR's documents were not analysed for it;
--   - an end that names a rework no reversal of the house in the consolidation carries is refused as before
--     (membership_entry_rework_link), and so is one that names none for a house that a rework did unload.
-- An entry that ends while the consolidation is OPEN names no rework, and every other end and every other state
-- stays frozen (membership_entry_frozen), as in 000008. No column changes: end_rework_execution_id and end_rework_no
-- are NULL together on the entry of a house that left with no rework, as on every entry that ends in an OPEN
-- consolidation.
--
-- What the database does not keep is what the domain keeps, as in 000008 (the reason is an operator's, a transfer's
-- target is OPEN and of the same mode and admits the house, the named rework is the latest, the end is not dated before
-- the unloading, the house was unloaded whole), and, new here, that a leaving with no rework is not dated before the
-- house's latest reversal in the consolidation (the application reads it under the same locks as the rework; the
-- database keeps only that the end is not before the entry's start), and this: that the house was shut out or unloaded at all. A house that
-- is still loaded is refused by its live packs, and one with none is let go, so the database cannot tell a house that
-- was never loaded from one that was taken out by ordinary reversals, and does not need to. The entry of a house that
-- left with no rework carries no record of why beyond the operator's reason; the application reads the rework when it
-- ends the entry, and records none for a house that was unloaded by a rework of another consolidation or of another
-- house, which this check cannot see either.
--
-- Objects of earlier migrations replaced here, each with every rule of its latest definition (file of that
-- definition):
--   cargo_phy.check_membership_entry()   000008_phy_house_leaves_after_rework
--
-- Not replaced, and checked: check_membership_entry_insert (000008); check_membership_entry_loaded (000002), which
-- still refuses at commit an entry that ended with live packs; check_live_entry_open (000001), which still refuses a
-- live entry, a transfer's new one included, in a consolidation that is not OPEN.
--
-- Locks: CREATE OR REPLACE FUNCTION locks no table. No table changes and no row is rewritten.

-- An entry is only ended, once. While its consolidation is CLOSED (the house cut-off), only a cancelled shipment's
-- entry may end, and the entry of a house with no live packs left in it, as DETACHED or TRANSFERRED: naming the rework
-- that unloaded it, or, in a sea consolidation, naming none when no rework unloaded it (V2.87). An entry that ends
-- while the consolidation is OPEN names no rework. The locks are those of 000008, in the order of every command
-- (application/ports.go: consolidations, load executions, membership ledgers, cargo lots): the consolidation FOR SHARE
-- first, then the house's ledger FOR NO KEY UPDATE without waiting, and only then are the house's reversals and load
-- read. A command has locked both before it updates the entry. A writer outside the commands locks the entry's row by
-- its UPDATE before this trigger runs, and a command holds the ledger and waits for that row, so a trigger that waited
-- for the ledger would deadlock with it; a writer that meets a command, or a load of the house, is refused at once with
-- lock_not_available, and one that locks the consolidation and the ledger itself first, as the commands do, never
-- meets it. The reversals that name a rework never change.
CREATE OR REPLACE FUNCTION cargo_phy.check_membership_entry() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
  ends text[] := ARRAY['end_kind','end_reason','end_text','ended_by','ended_at','end_entered_at','successor_no',
    'end_rework_execution_id','end_rework_no'];
  s text;
  k text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'cargo_phy: membership entries are kept' USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.ended_at IS NOT NULL OR NEW.ended_at IS NULL OR to_jsonb(OLD) - ends IS DISTINCT FROM to_jsonb(NEW) - ends THEN
    RAISE EXCEPTION 'cargo_phy: membership entry %/%/% can only be ended, once', OLD.tenant_id, OLD.shipment_id, OLD.entry_no
      USING ERRCODE = 'restrict_violation';
  END IF;
  SELECT state, kind INTO s, k FROM cargo_phy.consolidation WHERE tenant_id = NEW.tenant_id AND consolidation_id = NEW.consolidation_id FOR SHARE;
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
    IF NEW.end_rework_no IS NOT NULL OR EXISTS (SELECT 1 FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line l
         ON l.tenant_id = r.tenant_id AND l.execution_id = r.execution_id AND l.line_no = r.line_no
         WHERE r.tenant_id = NEW.tenant_id AND r.rework_no IS NOT NULL
           AND l.consolidation_id = NEW.consolidation_id AND l.shipment_id = NEW.shipment_id) THEN
      IF NOT EXISTS (SELECT 1 FROM cargo_phy.reversal_line r JOIN cargo_phy.load_line l
           ON l.tenant_id = r.tenant_id AND l.execution_id = r.execution_id AND l.line_no = r.line_no
           WHERE r.tenant_id = NEW.tenant_id AND r.execution_id = NEW.end_rework_execution_id AND r.rework_no = NEW.end_rework_no
             AND l.consolidation_id = NEW.consolidation_id AND l.shipment_id = NEW.shipment_id) THEN
        RAISE EXCEPTION 'cargo_phy: house %/% was unloaded in a rework of consolidation %/% and the entry does not name one of them, or it names one that did not unload the house; its entry is frozen',
          NEW.tenant_id, NEW.shipment_id, NEW.tenant_id, NEW.consolidation_id USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_rework_link';
      END IF;
    ELSIF NOT coalesce(k IN ('SEA_LCL_OWN','SEA_LCL_COLOAD'), false) THEN
      RAISE EXCEPTION 'cargo_phy: house %/% was unloaded in no rework of the closed consolidation % of kind %; its entry is frozen', NEW.tenant_id, NEW.shipment_id,
        NEW.consolidation_id, k USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_closed_sea_only';
    END IF;
    IF cargo_phy.live_packs_of_house(NEW.tenant_id, NEW.consolidation_id, NEW.shipment_id) > 0 THEN
      RAISE EXCEPTION 'cargo_phy: house %/% still has packs loaded in the closed consolidation %', NEW.tenant_id, NEW.shipment_id, NEW.consolidation_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'membership_entry_rework_loaded';
    END IF;
  END IF;
  RETURN NEW;
END $fn$;
