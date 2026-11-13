import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/money/money.dart';
import '../../../core/widgets/app_shell.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/money_widgets.dart';
import '../domain/models.dart';

class ActivityState {
  const ActivityState({this.query = const ActivityQuery(), this.items = const [], this.cursor, this.loading = false, this.loadingMore = false, this.error});
  final ActivityQuery query;
  final List<Activity> items;
  final String? cursor;
  final bool loading;
  final bool loadingMore;
  final Object? error;

  ActivityState copyWith({ActivityQuery? query, List<Activity>? items, String? cursor, bool clearCursor = false, bool? loading, bool? loadingMore, Object? error, bool clearError = false}) =>
      ActivityState(
        query: query ?? this.query,
        items: items ?? this.items,
        cursor: clearCursor ? null : (cursor ?? this.cursor),
        loading: loading ?? this.loading,
        loadingMore: loadingMore ?? this.loadingMore,
        error: clearError ? null : (error ?? this.error),
      );
}

final activityControllerProvider = NotifierProvider.autoDispose<ActivityController, ActivityState>(ActivityController.new);

class ActivityController extends AutoDisposeNotifier<ActivityState> {
  int _generation = 0;

  @override
  ActivityState build() {
    Future.microtask(reload);
    return const ActivityState(loading: true);
  }

  Future<void> setQuery(ActivityQuery q) async {
    if (q == state.query) return;
    state = state.copyWith(query: q);
    await reload();
  }

  Future<void> reload() async {
    final gen = ++_generation;
    state = state.copyWith(loading: true, clearError: true);
    try {
      final page = await ref.read(transactionsRepositoryProvider).list(state.query);
      if (gen != _generation) return; // a newer query superseded this one
      state = state.copyWith(items: page.items, cursor: page.nextCursor, clearCursor: page.nextCursor == null, loading: false);
    } catch (e) {
      if (gen != _generation) return;
      state = state.copyWith(loading: false, error: e);
    }
  }

  Future<void> loadMore() async {
    if (state.cursor == null || state.loadingMore || state.loading) return;
    final gen = _generation;
    state = state.copyWith(loadingMore: true);
    try {
      final page = await ref.read(transactionsRepositoryProvider).list(state.query, cursor: state.cursor);
      if (gen != _generation) return;
      state = state.copyWith(items: [...state.items, ...page.items], cursor: page.nextCursor, clearCursor: page.nextCursor == null, loadingMore: false);
    } catch (e) {
      state = state.copyWith(loadingMore: false, error: e);
    }
  }
}

class ActivityScreen extends ConsumerStatefulWidget {
  const ActivityScreen({super.key});
  @override
  ConsumerState<ActivityScreen> createState() => _ActivityScreenState();
}

class _ActivityScreenState extends ConsumerState<ActivityScreen> {
  final _search = TextEditingController();
  Timer? _debounce;

  static const _types = <(String?, String)>[(null, 'All'), ('deposit', 'Deposits'), ('transfer', 'Transfers'), ('withdrawal', 'Withdrawals')];
  static const _statuses = <(String?, String)>[(null, 'Any status'), ('pending', 'Pending'), ('succeeded', 'Successful'), ('failed', 'Failed')];

  @override
  void dispose() {
    _debounce?.cancel();
    _search.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final st = ref.watch(activityControllerProvider);
    final ctl = ref.read(activityControllerProvider.notifier);
    final grouped = <String, List<Activity>>{};
    for (final a in st.items) {
      grouped.putIfAbsent(dayLabel(a.createdAt), () => []).add(a);
    }
    return Scaffold(
      appBar: BrandHeader(title: 'Activity Ledger', actions: [
        IconButton(tooltip: 'Statements', onPressed: () => context.push('/statements'), icon: const Icon(Icons.download_outlined)),
      ]),
      body: NotificationListener<ScrollNotification>(
        onNotification: (n) {
          if (n.metrics.pixels > n.metrics.maxScrollExtent - 300) ctl.loadMore();
          return false;
        },
        child: RefreshIndicator(
          onRefresh: ctl.reload,
          child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
            TextField(
              key: const Key('activity.search'),
              controller: _search,
              decoration: const InputDecoration(prefixIcon: Icon(Icons.search), hintText: 'Search by name or reference'),
              onChanged: (v) {
                _debounce?.cancel();
                _debounce = Timer(const Duration(milliseconds: 350), () => ctl.setQuery(st.query.copyWith(search: v)));
              },
            ),
            const SizedBox(height: AppSpace.md),
            SingleChildScrollView(
              scrollDirection: Axis.horizontal,
              child: Row(children: [
                for (final (value, label) in _types)
                  Padding(
                    padding: const EdgeInsets.only(right: AppSpace.sm),
                    child: ChoiceChip(
                      key: Key('filter.type.${value ?? 'all'}'),
                      label: Text(label),
                      selected: st.query.type == value,
                      labelStyle: AppText.labelMd.copyWith(color: st.query.type == value ? Colors.white : AppColors.primary),
                      showCheckmark: false,
                      onSelected: (_) => ctl.setQuery(value == null ? st.query.copyWith(clearType: true) : st.query.copyWith(type: value)),
                    ),
                  ),
                const SizedBox(width: AppSpace.sm),
                DropdownButton<String?>(
                  value: st.query.status,
                  underline: const SizedBox.shrink(),
                  items: [for (final (v, l) in _statuses) DropdownMenuItem(value: v, child: Text(l, style: AppText.labelMd))],
                  onChanged: (v) => ctl.setQuery(v == null ? st.query.copyWith(clearStatus: true) : st.query.copyWith(status: v)),
                ),
              ]),
            ),
            const SizedBox(height: AppSpace.md),
            if (st.loading && st.items.isEmpty)
              const Padding(padding: EdgeInsets.all(AppSpace.xl), child: LoadingView())
            else if (st.error != null && st.items.isEmpty)
              ErrorView(error: st.error!, onRetry: ctl.reload)
            else if (st.items.isEmpty)
              const EmptyView(message: 'No transactions match your filters.')
            else
              for (final entry in grouped.entries) ...[
                Padding(
                  padding: const EdgeInsets.only(top: AppSpace.sm, bottom: AppSpace.sm),
                  child: Row(children: [
                    Expanded(child: Text(entry.key, style: AppText.labelLg.copyWith(color: AppColors.secondary))),
                    Text('${entry.value.length} actions', style: AppText.labelMd.copyWith(color: AppColors.muted)),
                  ]),
                ),
                for (final a in entry.value)
                  Padding(
                    padding: const EdgeInsets.only(bottom: AppSpace.sm),
                    child: TransactionRow(a, onTap: () => context.push('/transactions/${a.id}')),
                  ),
              ],
            if (st.loadingMore) const Padding(padding: EdgeInsets.all(AppSpace.md), child: LoadingView()),
            const SizedBox(height: AppSpace.xl),
          ]),
        ),
      ),
    );
  }
}

final receiptProvider = FutureProvider.autoDispose.family<Receipt, String>((ref, id) => ref.watch(transactionsRepositoryProvider).receipt(id));

class TransactionDetailScreen extends ConsumerWidget {
  const TransactionDetailScreen({super.key, required this.id});
  final String id;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final r = ref.watch(receiptProvider(id));
    return Scaffold(
      appBar: AppBar(title: const Text('Transaction receipt')),
      body: r.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(receiptProvider(id))),
        data: (rc) => ReceiptView(receipt: rc),
      ),
    );
  }
}

class ReceiptView extends StatelessWidget {
  const ReceiptView({super.key, required this.receipt});
  final Receipt receipt;

  @override
  Widget build(BuildContext context) {
    final a = receipt.activity;
    final rows = <(String, String)>[
      ('Receipt no.', receipt.receiptNumber),
      ('Type', activityTitle(a)),
      if (a.counterparty != null) (a.isCredit ? 'From' : 'To', a.counterparty!),
      if (a.counterpartyDetail != null) ('Account / phone', a.counterpartyDetail!),
      if (a.provider != null) ('Channel', providerLabel(a.provider)),
      if (a.reference != null) ('Provider reference', a.reference!),
      if (a.description.isNotEmpty && a.type != 'adjustment') ('Note', a.description),
      ('Initiated', formatDateTime(a.createdAt)),
      if (a.completedAt != null) ('Completed', formatDateTime(a.completedAt!)),
      ('Wallet', receipt.walletAccount),
      if (receipt.environment == 'simulated') ('Environment', 'SIMULATED - no real money moved'),
      if (a.failureReason != null) ('Reason', a.failureReason!),
    ];
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      AppCard(
        padding: const EdgeInsets.all(AppSpace.lg),
        child: Column(children: [
          CircleAvatar(radius: 28, backgroundColor: a.isCredit ? AppColors.successBg : AppColors.outboundBg, child: Icon(activityIcon(a), color: a.isCredit ? AppColors.success : AppColors.primary)),
          const SizedBox(height: AppSpace.md),
          MoneyText(a.amount, key: const Key('receipt.amount'), sign: a.isCredit ? '+' : '-', style: AppText.headlineLg),
          const SizedBox(height: AppSpace.sm),
          StatusChip(a.displayStatus),
          if (a.displayStatus == DisplayStatus.unknown || receipt.outcomeUncertain)
            Padding(
              padding: const EdgeInsets.only(top: AppSpace.md),
              child: Text('We are confirming the outcome with the provider. Your funds are safe and held until it is confirmed.',
                  textAlign: TextAlign.center, style: AppText.bodySm.copyWith(color: AppColors.secondary)),
            ),
          const Divider(height: AppSpace.xl),
          for (final (k, v) in rows)
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 6),
              child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                SizedBox(width: 130, child: Text(k, style: AppText.bodyMd.copyWith(color: AppColors.muted))),
                Expanded(child: SelectableText(v, style: AppText.labelLg, textAlign: TextAlign.end)),
              ]),
            ),
          if (!a.fee.isZero) _moneyRow('Fee', a.fee),
          if (receipt.total != null) _moneyRow('Total debited', receipt.total!),
        ]),
      ),
      const SizedBox(height: AppSpace.md),
      OutlinedButton.icon(
        onPressed: () {
          Clipboard.setData(ClipboardData(text: rows.map((r) => '${r.$1}: ${r.$2}').join('\n')));
          showSnack(context, 'Receipt copied');
        },
        icon: const Icon(Icons.copy),
        label: const Text('Copy receipt'),
      ),
    ]);
  }

  Widget _moneyRow(String label, Money m) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Row(children: [
          Expanded(child: Text(label, style: AppText.bodyMd.copyWith(color: AppColors.muted))),
          MoneyText(m, style: AppText.labelLg),
        ]),
      );
}
