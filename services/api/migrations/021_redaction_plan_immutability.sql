CREATE FUNCTION enforce_redaction_plan_immutability() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' AND EXISTS (
    SELECT 1 FROM sessions WHERE id=OLD.session_id AND state='DELETED'
  ) THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'redaction plans are immutable outside governed session erasure';
END
$$;

CREATE TRIGGER redaction_plans_immutable
BEFORE UPDATE OR DELETE ON redaction_plans
FOR EACH ROW EXECUTE FUNCTION enforce_redaction_plan_immutability();
