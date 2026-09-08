-- Reconsider retained output failures once after fixing LAPSE cue ordering.
-- Newly observed invalid output remains subject to normal retained rejection.
DELETE FROM candidate_rejections WHERE reason_code = 'lapse_invalid_output';
