import { getUserById } from "./user";

// Line 5 (0-based) puts eight non-BMP characters before two symbols, chosen so
// that a byte-offset or codepoint-offset misreading of the column lands outside
// the intended identifier. See testdata/README.md.
export const ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; export const rocketName = getUserById("42").name;
