-- One campaign-wide parameter map served every recipient, so a value that
-- differs per row -- a coupon code, a tracking token -- had nowhere to live.
-- A recipient's map overrides the campaign's map for the same slot only. The
-- slot shape stays frozen at campaign creation, so the approved render
-- contract cannot change per recipient.
ALTER TABLE campaign_recipients ADD COLUMN template_params_json TEXT NOT NULL DEFAULT '{}';
