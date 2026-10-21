-- Installation-time coverage and overrun failures of LAPSE output were stored
-- as partial_coverage or invalid_subtitle, and those reasons now survive LAPSE
-- upgrades. Old rows cannot say whether LAPSE's timing caused them, so
-- reconsider every system rejection with either reason once. The owner's
-- user_* rejections and every other reason are kept.
DELETE FROM candidate_rejections WHERE reason_code IN ('partial_coverage', 'invalid_subtitle');
