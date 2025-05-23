// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import React from "react";
import * as protos from "@cockroachlabs/crdb-protobuf-client";
import {
  SortedTable,
  ISortedTablePagination,
  longListWithTooltip,
  ColumnDescriptor,
} from "../sortedtable";
import {
  transactionsCountBarChart,
  transactionsBytesReadBarChart,
  transactionsLatencyBarChart,
  transactionsContentionBarChart,
  transactionsMaxMemUsageBarChart,
  transactionsNetworkBytesBarChart,
  transactionsRetryBarChart,
} from "./transactionsBarCharts";
import { statisticsTableTitles } from "../statsTableUtil/statsTableUtil";
import { tableClasses } from "./transactionsTableClasses";
import { transactionLink } from "./transactionsCells";
import {
  FixFingerprintHexValue,
  Count,
  FixLong,
  longToInt,
  TimestampToString,
  unset,
} from "src/util";
import { SortSetting } from "../sortedtable";
import {
  getStatementsByFingerprintId,
  collectStatementsText,
  statementFingerprintIdsToText,
  statementFingerprintIdsToSummarizedText,
} from "../transactionsPage/utils";
import classNames from "classnames/bind";
import statsTablePageStyles from "src/statementsTable/statementsTableContent.module.scss";

export type Transaction = protos.cockroach.server.serverpb.StatementsResponse.IExtendedCollectedTransactionStatistics;
type Statement = protos.cockroach.server.serverpb.StatementsResponse.ICollectedStatementStatistics;

interface TransactionsTable {
  transactions: TransactionInfo[];
  sortSetting: SortSetting;
  onChangeSortSetting: (ss: SortSetting) => void;
  pagination: ISortedTablePagination;
  renderNoResult?: React.ReactNode;
  columns: ColumnDescriptor<TransactionInfo>[];
}

export interface TransactionInfo extends Transaction {
  regionNodes: string[];
}

const { latencyClasses } = tableClasses;

const cx = classNames.bind(statsTablePageStyles);

interface TransactionLinkTargetProps {
  aggregatedTs: string;
  transactionFingerprintId: string;
}

// TransactionLinkTarget returns the link to the relevant transaction page, given
// the input transaction details.
export const TransactionLinkTarget = (
  props: TransactionLinkTargetProps,
): string => {
  return `/transaction/${props.transactionFingerprintId}`;
};

export function makeTransactionsColumns(
  transactions: TransactionInfo[],
  statements: Statement[],
  isTenant: boolean,
  search?: string,
): ColumnDescriptor<TransactionInfo>[] {
  const defaultBarChartOptions = {
    classes: {
      root: cx("statements-table__col--bar-chart"),
      label: cx("statements-table__col--bar-chart__label"),
    },
  };
  const sampledExecStatsBarChartOptions = {
    classes: defaultBarChartOptions.classes,
    displayNoSamples: (d: TransactionInfo) => {
      return longToInt(d.stats_data.stats.exec_stats?.count) == 0;
    },
  };

  const countBar = transactionsCountBarChart(transactions);
  const bytesReadBar = transactionsBytesReadBarChart(
    transactions,
    defaultBarChartOptions,
  );
  const latencyBar = transactionsLatencyBarChart(
    transactions,
    latencyClasses.barChart,
  );
  const contentionBar = transactionsContentionBarChart(
    transactions,
    sampledExecStatsBarChartOptions,
  );
  const maxMemUsageBar = transactionsMaxMemUsageBarChart(
    transactions,
    sampledExecStatsBarChartOptions,
  );
  const networkBytesBar = transactionsNetworkBytesBarChart(
    transactions,
    sampledExecStatsBarChartOptions,
  );
  const retryBar = transactionsRetryBarChart(transactions);

  const statType = "transaction";
  return [
    {
      name: "transactions",
      title: statisticsTableTitles.transactions(statType),
      cell: (item: TransactionInfo) =>
        transactionLink({
          transactionText:
            statementFingerprintIdsToText(
              item.stats_data.statement_fingerprint_ids,
              statements,
            ) || "Transaction query unavailable.",
          transactionSummary:
            statementFingerprintIdsToSummarizedText(
              item.stats_data.statement_fingerprint_ids,
              statements,
            ) || "Transaction query unavailable.",
          aggregatedTs: TimestampToString(item.stats_data.aggregated_ts),
          transactionFingerprintId: item.stats_data.transaction_fingerprint_id.toString(),
          search,
        }),
      sort: (item: TransactionInfo) =>
        collectStatementsText(
          getStatementsByFingerprintId(
            item.stats_data.statement_fingerprint_ids,
            statements,
          ),
        ),
      alwaysShow: true,
    },
    {
      name: "executionCount",
      title: statisticsTableTitles.executionCount(statType),
      cell: countBar,
      sort: (item: TransactionInfo) =>
        FixLong(Number(item.stats_data.stats.count)),
    },
    {
      name: "applicationName",
      title: statisticsTableTitles.applicationName(statType),
      className: cx("statements-table__col-app-name"),
      cell: (item: TransactionInfo) =>
        item.stats_data?.app?.length > 0 ? item.stats_data?.app : unset,
      sort: (item: TransactionInfo) => item.stats_data?.app,
      showByDefault: false,
    },
    {
      name: "rowsProcessed",
      title: statisticsTableTitles.rowsProcessed(statType),
      cell: (item: TransactionInfo) =>
        `${Count(Number(item.stats_data.stats.rows_read.mean))} Reads / ${Count(
          Number(item.stats_data.stats.rows_written?.mean),
        )} Writes`,
      className: cx("statements-table__col-rows-read"),
      sort: (item: TransactionInfo) =>
        FixLong(
          Number(item.stats_data.stats.rows_read.mean) +
            Number(item.stats_data.stats.rows_written?.mean),
        ),
    },
    {
      name: "bytesRead",
      title: statisticsTableTitles.bytesRead(statType),
      cell: bytesReadBar,
      className: cx("statements-table__col-bytes-read"),
      sort: (item: TransactionInfo) =>
        FixLong(Number(item.stats_data.stats.bytes_read.mean)),
    },
    {
      name: "time",
      title: statisticsTableTitles.time(statType),
      cell: latencyBar,
      className: latencyClasses.column,
      sort: (item: TransactionInfo) => item.stats_data.stats.service_lat.mean,
    },
    {
      name: "contention",
      title: statisticsTableTitles.contention(statType),
      cell: contentionBar,
      className: cx("statements-table__col-contention"),
      sort: (item: TransactionInfo) =>
        FixLong(Number(item.stats_data.stats.exec_stats.contention_time?.mean)),
    },
    {
      name: "maxMemUsage",
      title: statisticsTableTitles.maxMemUsage(statType),
      cell: maxMemUsageBar,
      className: cx("statements-table__col-max-mem-usage"),
      sort: (item: TransactionInfo) =>
        FixLong(Number(item.stats_data.stats.exec_stats.max_mem_usage?.mean)),
    },
    {
      name: "networkBytes",
      title: statisticsTableTitles.networkBytes(statType),
      cell: networkBytesBar,
      className: cx("statements-table__col-network-bytes"),
      sort: (item: TransactionInfo) =>
        FixLong(Number(item.stats_data.stats.exec_stats.network_bytes?.mean)),
    },
    {
      name: "retries",
      title: statisticsTableTitles.retries(statType),
      cell: retryBar,
      sort: (item: TransactionInfo) =>
        longToInt(Number(item.stats_data.stats.max_retries)),
    },
    {
      name: "regionNodes",
      title: statisticsTableTitles.regionNodes(statType),
      className: cx("statements-table__col-regions"),
      cell: (item: TransactionInfo) => {
        return longListWithTooltip(item.regionNodes.sort().join(", "), 50);
      },
      sort: (item: TransactionInfo) => item.regionNodes.sort().join(", "),
      hideIfTenant: true,
    },
    {
      name: "statementsCount",
      title: statisticsTableTitles.statementsCount(statType),
      cell: (item: TransactionInfo) =>
        item.stats_data.statement_fingerprint_ids.length,
      sort: (item: TransactionInfo) =>
        item.stats_data.statement_fingerprint_ids.length,
    },
    {
      name: "transactionFingerprintId",
      title: statisticsTableTitles.transactionFingerprintId(statType),
      cell: (item: TransactionInfo) =>
        FixFingerprintHexValue(
          item.stats_data?.transaction_fingerprint_id.toString(16),
        ),
      sort: (item: TransactionInfo) =>
        item.stats_data?.transaction_fingerprint_id.toString(16),
      showByDefault: false,
    },
  ].filter(c => !(isTenant && c.hideIfTenant));
}

export const TransactionsTable: React.FC<TransactionsTable> = props => {
  const { transactions, columns } = props;

  return (
    <SortedTable
      data={transactions}
      columns={columns}
      className="statements-table"
      {...props}
    />
  );
};

TransactionsTable.defaultProps = {};
