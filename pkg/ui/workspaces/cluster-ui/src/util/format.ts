// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { longToInt } from "./fixLong";

export const kibi = 1024;
export const byteUnits: string[] = [
  "B",
  "KiB",
  "MiB",
  "GiB",
  "TiB",
  "PiB",
  "EiB",
  "ZiB",
  "YiB",
];
export const durationUnits: string[] = ["ns", "µs", "ms", "s"];
export const countUnits: string[] = ["", "k", "m", "b"];

interface UnitValue {
  value: number;
  units: string;
}

// computePrefixExponent is used to compute an appropriate display unit for a value
// for which the units have metric-style prefixes available. For example, the
// value may be expressed in bytes, but we may want to display it on the graph
// as a larger prefix unit (such as "kilobytes" or "gigabytes") in order to make
// the numbers more readable.
export function ComputePrefixExponent(
  value: number,
  prefixMultiple: number,
  prefixList: string[],
): number {
  // Compute the metric prefix that will be used to label the axis.
  let maxUnits = Math.abs(value);
  let prefixScale: number;
  for (
    prefixScale = 0;
    maxUnits >= prefixMultiple && prefixScale < prefixList.length - 1;
    prefixScale++
  ) {
    maxUnits /= prefixMultiple;
  }
  return prefixScale;
}

/**
 * ComputeByteScale calculates the appropriate scale factor and unit to use to
 * display a given byte value, without actually converting the value.
 *
 * This is used to prescale byte values before passing them to a d3-axis.
 */
export function ComputeByteScale(bytes: number): UnitValue {
  const scale = ComputePrefixExponent(bytes, kibi, byteUnits);
  return {
    value: Math.pow(kibi, scale),
    units: byteUnits[scale],
  };
}

export function BytesToUnitValue(bytes: number): UnitValue {
  const scale = ComputeByteScale(bytes);
  return {
    value: bytes / scale.value,
    units: scale.units,
  };
}

/**
 * BytesWithPrecision is like Bytes, but accepts a precision parameter
 * indicating how many digits after the decimal point are desired.
 */
export function BytesWithPrecision(bytes: number, precision: number): string {
  const unitVal = BytesToUnitValue(bytes);
  if (!unitVal.value) {
    return "0 B";
  }
  return unitVal.value.toFixed(precision) + " " + unitVal.units;
}

/**
 * Bytes creates a string representation for a number of bytes. For
 * large numbers of bytes, the value will be converted into a large unit
 * (e.g. Kibibytes, Mebibytes).
 *
 * This function was adapted from
 * https://stackoverflow.com/questions/10420352/converting-file-size-in-bytes-to-human-readable
 */
export function Bytes(bytes: number): string {
  return BytesWithPrecision(bytes, 1);
}

/**
 * Cast bytes to provided scale units
 */
// tslint:disable-next-line: variable-name
export const BytesFitScale = (scale: string) => (bytes: number): string => {
  if (!bytes) {
    return `0.00 ${scale}`;
  }
  const n = byteUnits.indexOf(scale);
  return `${(bytes / Math.pow(kibi, n)).toFixed(2)} ${scale}`;
};

/**
 * Percentage creates a string representation of a fraction as a percentage.
 * Accepts a precision parameter as optional indicating how many digits
 * after the decimal point are desired. (e.g. precision 2 returns 8.37 %)
 */
export function Percentage(
  numerator: number,
  denominator: number,
  precision?: number,
): string {
  if (denominator === 0) {
    return "--%";
  }
  if (precision) {
    return ((numerator / denominator) * 100).toFixed(precision) + " %";
  }
  return Math.floor((numerator / denominator) * 100).toString() + " %";
}

/**
 * ComputeDurationScale calculates an appropriate scale factor and unit to use
 * to display a given duration value, without actually converting the value.
 */
export function ComputeDurationScale(nanoseconds: number): UnitValue {
  const scale = ComputePrefixExponent(nanoseconds, 1000, durationUnits);
  return {
    value: Math.pow(1000, scale),
    units: durationUnits[scale],
  };
}

/**
 * Duration creates a string representation for a duration. The expectation is
 * that units are passed in nanoseconds; for larger durations, the value will
 * be converted into larger units.
 */
export function Duration(nanoseconds: number): string {
  const scale = ComputeDurationScale(nanoseconds);
  const unitVal = nanoseconds / scale.value;
  return unitVal.toFixed(1) + " " + scale.units;
}

/**
 * Cast nanoseconds to provided scale units
 */
// tslint:disable-next-line: variable-name
export const DurationFitScale = (scale: string) => (
  nanoseconds: number,
): string => {
  if (!nanoseconds) {
    return `0.00 ${scale}`;
  }
  const n = durationUnits.indexOf(scale);
  return `${(nanoseconds / Math.pow(1000, n)).toFixed(2)} ${scale}`;
};

export const DATE_FORMAT = "MMM DD, YYYY [at] H:mm";
export const DATE_FORMAT_24_UTC = "MMM DD, YYYY [at] H:mm UTC";
export const DATE_WITH_SECONDS_FORMAT_24_UTC = "MMM DD, YYYY [at] H:mm:ss UTC";

export function RenderCount(yesCount: Long, totalCount: Long): string {
  if (longToInt(yesCount) == 0) {
    return "No";
  }
  if (longToInt(yesCount) == longToInt(totalCount)) {
    return "Yes";
  }
  const noCount = longToInt(totalCount) - longToInt(yesCount);
  return `${longToInt(yesCount)} Yes / ${noCount} No`;
}

/**
 * ComputeCountScale calculates an appropriate scale factor and unit to use
 * to display a given count value, without actually converting the value.
 */
function ComputeCountScale(count: number): UnitValue {
  const scale = ComputePrefixExponent(count, 1000, countUnits);
  return {
    value: Math.pow(1000, scale),
    units: countUnits[scale],
  };
}

/**
 * Count creates a string representation for a count.
 */
export function Count(count: number): string {
  const scale = ComputeCountScale(count);
  const unitVal = count / scale.value;
  const fractionDigits = Number.isInteger(unitVal) ? 0 : 1;
  return unitVal.toFixed(fractionDigits) + " " + scale.units;
}

// FixFingerprintHexValue adds the leading 0 on strings with hex value that
// have a length < 16. This can occur because it was returned like this from the DB
// or because the hex value was generated using `.toString(16)` (which removes the
// leading zeros).
// The zeros need to be added back to match the value on our sql stats tables.
export function FixFingerprintHexValue(s: string): string {
  if (s === undefined || s === null || s.length === 0) {
    return "";
  }
  while (s.length < 16) {
    s = `0${s}`;
  }
  return s;
}

interface BreakLineReplacement {
  [key: string]: string;
}

const breakLinesKeywords: BreakLineReplacement = {
  " FROM ": " FROM ",
  " WHERE ": "   WHERE ",
  " AND ": "    AND ",
  " ORDER ": " ORDER ",
  " LIMIT ": " LIMIT ",
  " JOIN ": "   JOIN ",
  " ON ": "    ON ",
  " VALUES ": "   VALUES ",
};
const LINE_BREAK_LIMIT = 100;

export function FormatQuery(query: string): string {
  if (query == null) {
    return "";
  }
  Object.keys(breakLinesKeywords).forEach(key => {
    query = query.replace(new RegExp(key, "g"), `\n${breakLinesKeywords[key]}`);
  });
  const lines = query.split("\n").map(line => {
    if (line.length <= LINE_BREAK_LIMIT) {
      return line;
    }
    return breakLongLine(line, LINE_BREAK_LIMIT);
  });

  return lines.join("\n");
}

function breakLongLine(line: string, limit: number): string {
  if (line.length <= limit) {
    return line;
  }
  const idxComma = line.indexOf(",", limit);
  if (idxComma == -1) {
    return line;
  }

  return `${line.substring(0, idxComma + 1)}\n${breakLongLine(
    line.substring(idxComma + 1).trim(),
    limit,
  )}`;
}
