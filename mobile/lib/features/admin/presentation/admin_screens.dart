import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/networking/api_error.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../auth/presentation/session_controller.dart';
import '../../transactions/domain/models.dart';
import '../data/admin_repository.dart';

final adminDashboardProvider = FutureProvider.autoDispose<Json>((ref) => ref.watch(adminRepositoryProvider).dashboard());
final adminProviderHealthProvider = FutureProvider.autoDispose<List<Json>>((ref) => ref.watch(adminRepositoryProvider).providerHealth());
final adminPaymentsFilterProvider = StateProvider.autoDispose<(String?, bool)>((_) => (null, true));
final adminPaymentsProvider = FutureProvider.autoDispose<List<Json>>((ref) {
  final (kind, review) = ref.watch(adminPaymentsFilterProvider);
  return ref.watch(adminRepositoryProvider).payments(kind: kind, needsReview: review);
});
final adminReconStatusProvider = StateProvider.autoDispose<String>((_) => 'open');
final adminReconProvider = FutureProvider.autoDispose<List<Json>>((ref) => ref.watch(adminRepositoryProvider).reconItems(status: ref.watch(adminReconStatusProvider)));
final adminRequestsProvider = FutureProvider.autoDispose<List<Json>>((ref) => ref.watch(adminRepositoryProvider).financialRequests());
final adminUserQueryProvider = StateProvider.autoDispose<String>((_) => '');
final adminUsersProvider = FutureProvider.autoDispose<List<Json>>((ref) => ref.watch(adminRepositoryProvider).searchUsers(ref.watch(adminUserQueryProvider)));
final adminUserProvider = FutureProvider.autoDispose.family<Json, String>((ref, id) => ref.watch(adminRepositoryProvider).user(id));
final adminAuditActionProvider = StateProvider.autoDispose<String>((_) => '');
final adminAuditProvider = FutureProvider.autoDispose<List<Json>>((ref) => ref.watch(adminRepositoryProvider).auditEvents(action: ref.watch(adminAuditActionProvider)));

/// Admin console layout: navigation rail on wide screens, drawer on phones.
class AdminShell extends ConsumerWidget {
  const AdminShell({super.key, required this.child, required this.location});
  final Widget child;
  final String location;

  static const destinations = [
    ('/admin', Icons.dashboard_outlined, 'Dashboard'),
    ('/admin/payments', Icons.payments_outlined, 'Payments'),
    ('/admin/reconciliation', Icons.rule_folder_outlined, 'Reconciliation'),
    ('/admin/approvals', Icons.fact_check_outlined, 'Approvals'),
    ('/admin/users', Icons.people_outline, 'Users'),
    ('/admin/audit', Icons.history_edu_outlined, 'Audit log'),
  ];

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final user = ref.watch(currentUserProvider);
    final index = destinations.lastIndexWhere((d) => location == d.$1 || location.startsWith('${d.$1}/'));
    final wide = MediaQuery.sizeOf(context).width >= 900;
    final rail = NavigationRail(
      selectedIndex: index < 0 ? 0 : index,
      extended: wide,
      backgroundColor: AppColors.surface,
      labelType: wide ? NavigationRailLabelType.none : NavigationRailLabelType.all,
      onDestinationSelected: (i) => context.go(destinations[i].$1),
      destinations: [for (final d in destinations) NavigationRailDestination(icon: Icon(d.$2), label: Text(d.$3))],
    );
    return Scaffold(
      appBar: AppBar(
        title: Text('Operations Console', style: AppText.headlineSm),
        actions: [
          if (user != null)
            Padding(
              padding: const EdgeInsets.only(right: AppSpace.sm),
              child: Center(child: Text('${user.fullName} • ${user.role}', style: AppText.labelMd.copyWith(color: AppColors.secondary))),
            ),
          IconButton(tooltip: 'Sign out', onPressed: () => ref.read(sessionProvider.notifier).logout(), icon: const Icon(Icons.logout)),
        ],
      ),
      drawer: MediaQuery.sizeOf(context).width < 600 ? Drawer(child: SafeArea(child: rail)) : null,
      body: Row(children: [
        if (MediaQuery.sizeOf(context).width >= 600) rail,
        if (MediaQuery.sizeOf(context).width >= 600) const VerticalDivider(width: 1),
        Expanded(child: child),
      ]),
    );
  }
}

String _m(Object? v) => v == null ? '-' : 'KES ${v.toString()}';

Widget _kv(String k, Object? v) => Padding(
      padding: const EdgeInsets.symmetric(vertical: 2),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        SizedBox(width: 150, child: Text(k, style: AppText.bodySm.copyWith(color: AppColors.muted))),
        Expanded(child: SelectableText('${v ?? '-'}', style: AppText.bodyMd)),
      ]),
    );

Future<String?> _promptText(BuildContext context, {required String title, required String label, String? hint, int minLength = 10}) async {
  final c = TextEditingController();
  return showDialog<String>(
    context: context,
    builder: (ctx) => AlertDialog(
      title: Text(title),
      content: TextField(controller: c, maxLines: 3, decoration: InputDecoration(labelText: label, hintText: hint)),
      actions: [
        TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Cancel')),
        FilledButton(
          onPressed: () {
            if (c.text.trim().length < minLength) return;
            Navigator.pop(ctx, c.text.trim());
          },
          child: const Text('Submit'),
        ),
      ],
    ),
  );
}

Future<void> _run(BuildContext context, Future<void> Function() action, String success) async {
  try {
    await action();
    if (context.mounted) showSnack(context, success);
  } on ApiException catch (e) {
    if (context.mounted) showSnack(context, e.message);
  }
}

class AdminDashboardScreen extends ConsumerWidget {
  const AdminDashboardScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final d = ref.watch(adminDashboardProvider);
    final health = ref.watch(adminProviderHealthProvider);
    return RefreshIndicator(
      onRefresh: () async {
        ref.invalidate(adminDashboardProvider);
        ref.invalidate(adminProviderHealthProvider);
      },
      child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
        d.when(
          loading: () => const LoadingView(),
          error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(adminDashboardProvider)),
          data: (j) {
            final consistent = j['ledger_consistent'] == true;
            final vol = (j['volume_24h'] as Map?) ?? {};
            return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
              AppCard(
                color: consistent ? AppColors.successBg : AppColors.dangerBg,
                child: Row(children: [
                  Icon(consistent ? Icons.verified : Icons.warning_amber, color: consistent ? AppColors.success : AppColors.danger),
                  const SizedBox(width: AppSpace.sm),
                  Expanded(
                    child: Text(consistent ? 'Ledger balanced: projections match entries' : 'LEDGER INCONSISTENCY - investigate reconciliation items',
                        style: AppText.titleMd.copyWith(color: consistent ? AppColors.success : AppColors.danger)),
                  ),
                ]),
              ),
              const SizedBox(height: AppSpace.md),
              Wrap(spacing: AppSpace.md, runSpacing: AppSpace.md, children: [
                _stat('Customer liabilities', _m(j['customer_liabilities'])),
                _stat('Users (active)', '${j['users']} (${j['active_users']})'),
                _stat('Pending deposits', '${j['pending_deposits']}'),
                _stat('Processing withdrawals', '${j['processing_withdrawals']}'),
                _stat('Needs review', '${j['needs_review']}', alert: (j['needs_review'] as num? ?? 0) > 0),
                _stat('Open recon items', '${j['open_reconciliation_items']}', alert: (j['open_reconciliation_items'] as num? ?? 0) > 0),
                _stat('Pending approvals', '${j['pending_financial_requests']}'),
                _stat('Dead jobs', '${j['dead_jobs']}', alert: (j['dead_jobs'] as num? ?? 0) > 0),
                _stat('Deposits 24h', _m(vol['deposits'])),
                _stat('Withdrawals 24h', _m(vol['withdrawals'])),
                _stat('Transfers 24h', _m(vol['transfers'])),
              ]),
            ]);
          },
        ),
        const SizedBox(height: AppSpace.lg),
        const SectionLabel('Provider integration health'),
        health.when(
          loading: () => const LoadingView(),
          error: (e, _) => ErrorView(error: e),
          data: (items) => Column(children: [
            for (final h in items)
              Padding(
                padding: const EdgeInsets.only(bottom: AppSpace.sm),
                child: AppCard(
                  child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                    Row(children: [
                      Text(providerLabel(h['provider'] as String?), style: AppText.titleMd),
                      const SizedBox(width: AppSpace.sm),
                      Chip(label: Text('${h['environment']}'.toUpperCase())),
                    ]),
                    _kv('Attempts 24h', h['attempts_24h']),
                    _kv('Succeeded / failed', '${h['succeeded_24h']} / ${h['failed_24h']}'),
                    _kv('Unknown outcome', h['unknown_24h']),
                    _kv('Last success', h['last_success_at']),
                    _kv('Last callback', h['last_callback_at']),
                    _kv('Callback errors 24h', h['callback_errors_24h']),
                    _kv('Capabilities', (h['capabilities'] as Map?)?.entries.where((e) => e.value == true).map((e) => e.key).join(', ')),
                  ]),
                ),
              ),
          ]),
        ),
      ]),
    );
  }

  Widget _stat(String label, String value, {bool alert = false}) => SizedBox(
        width: 200,
        child: AppCard(
          color: alert ? AppColors.pendingBg : null,
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(label, style: AppText.labelMd.copyWith(color: AppColors.secondary)),
            const SizedBox(height: 4),
            Text(value, style: AppText.headlineSm.copyWith(fontFeatures: tabular)),
          ]),
        ),
      );
}

class AdminPaymentsScreen extends ConsumerWidget {
  const AdminPaymentsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final (kind, review) = ref.watch(adminPaymentsFilterProvider);
    final list = ref.watch(adminPaymentsProvider);
    final canAct = ref.watch(currentUserProvider)?.isAdmin ?? false;
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      Wrap(spacing: AppSpace.sm, children: [
        for (final (v, l) in const [(null, 'All'), ('deposit', 'Deposits'), ('withdrawal', 'Withdrawals')])
          ChoiceChip(label: Text(l), selected: kind == v, onSelected: (_) => ref.read(adminPaymentsFilterProvider.notifier).state = (v, review)),
        FilterChip(label: const Text('Needs review / uncertain'), selected: review, onSelected: (s) => ref.read(adminPaymentsFilterProvider.notifier).state = (kind, s)),
      ]),
      const SizedBox(height: AppSpace.md),
      list.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(adminPaymentsProvider)),
        data: (rows) => rows.isEmpty
            ? const EmptyView(message: 'Nothing needs attention.')
            : Column(children: [
                for (final p in rows)
                  Padding(
                    padding: const EdgeInsets.only(bottom: AppSpace.sm),
                    child: AppCard(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Row(children: [
                          Expanded(child: Text('${p['kind']} • ${providerLabel(p['provider'] as String?)} • ${p['user_name']}', style: AppText.titleMd)),
                          StatusChip(DisplayStatus.parse(_display(p))),
                        ]),
                        _kv('Amount / fee', '${_m(p['amount'])} / ${_m(p['fee'])}'),
                        _kv('Status', '${p['status']} (provider: ${p['provider_status']})'),
                        _kv('Environment', p['provider_environment']),
                        _kv('Receipt', p['provider_receipt']),
                        _kv('Provider result', '${p['provider_result_code'] ?? ''} ${p['provider_result_desc'] ?? ''}'),
                        _kv('Failure reason', p['failure_reason']),
                        _kv('Age', '${((p['age_seconds'] as num) / 60).floor()} min'),
                        _kv('ID', p['id']),
                        if (canAct)
                          Wrap(spacing: AppSpace.sm, children: [
                            OutlinedButton(
                              onPressed: () => _run(context, () => ref.read(adminRepositoryProvider).recheck(p['kind'] as String, p['id'] as String), 'Status check scheduled'),
                              child: const Text('Recheck with provider'),
                            ),
                            if (p['kind'] == 'withdrawal' && p['status'] == 'processing')
                              OutlinedButton(onPressed: () => _resolve(context, ref, p), child: const Text('Resolve from evidence')),
                          ]),
                      ]),
                    ),
                  ),
              ]),
      ),
    ]);
  }

  static String _display(Json p) => switch (p['status']) {
        'succeeded' || 'failed' || 'reversed' => p['status'] as String,
        _ when p['outcome_uncertain'] == true => 'unknown',
        'expired' => 'unknown',
        _ => 'pending',
      };

  Future<void> _resolve(BuildContext context, WidgetRef ref, Json p) async {
    final outcome = await showDialog<String>(
      context: context,
      builder: (ctx) => SimpleDialog(title: const Text('Provider evidence shows the payout…'), children: [
        SimpleDialogOption(onPressed: () => Navigator.pop(ctx, 'succeeded'), child: const Text('SUCCEEDED (money reached customer)')),
        SimpleDialogOption(onPressed: () => Navigator.pop(ctx, 'failed'), child: const Text('FAILED (no money left the provider account)')),
      ]),
    );
    if (outcome == null || !context.mounted) return;
    String? receipt;
    if (outcome == 'succeeded') {
      receipt = await _promptText(context, title: 'Provider receipt', label: 'Receipt / transaction ID', minLength: 4);
      if (receipt == null) return;
    }
    if (!context.mounted) return;
    final evidence = await _promptText(context, title: 'Evidence', label: 'Describe the evidence (statement line, ticket)');
    if (evidence == null || !context.mounted) return;
    await _run(context, () => ref.read(adminRepositoryProvider).resolveWithdrawal(p['id'] as String, outcome: outcome, evidence: evidence, receipt: receipt), 'Withdrawal resolved');
    ref.invalidate(adminPaymentsProvider);
  }
}

class AdminReconciliationScreen extends ConsumerWidget {
  const AdminReconciliationScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final status = ref.watch(adminReconStatusProvider);
    final items = ref.watch(adminReconProvider);
    final canAct = ref.watch(currentUserProvider)?.isAdmin ?? false;
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      Row(children: [
        Expanded(
          child: Wrap(spacing: AppSpace.sm, children: [
            for (final s in const ['open', 'investigating', 'resolved', 'dismissed'])
              ChoiceChip(label: Text(s), selected: status == s, onSelected: (_) => ref.read(adminReconStatusProvider.notifier).state = s),
          ]),
        ),
        if (canAct)
          FilledButton.icon(
            style: FilledButton.styleFrom(minimumSize: const Size(0, 44)),
            onPressed: () async {
              await _run(context, () => ref.read(adminRepositoryProvider).runReconciliation(), 'Reconciliation run completed');
              ref.invalidate(adminReconProvider);
            },
            icon: const Icon(Icons.play_arrow),
            label: const Text('Run now'),
          ),
      ]),
      const SizedBox(height: AppSpace.sm),
      Text('Resolving an item records the investigation outcome; it never changes balances. Use Approvals for corrections.',
          style: AppText.bodySm.copyWith(color: AppColors.muted)),
      const SizedBox(height: AppSpace.md),
      items.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(adminReconProvider)),
        data: (rows) => rows.isEmpty
            ? const EmptyView(message: 'No discrepancies in this state.')
            : Column(children: [
                for (final it in rows)
                  Padding(
                    padding: const EdgeInsets.only(bottom: AppSpace.sm),
                    child: AppCard(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Text('${it['discrepancy']}'.replaceAll('_', ' ').toUpperCase(), style: AppText.labelLg.copyWith(color: AppColors.danger)),
                        _kv('Entity', '${it['entity_type'] ?? ''} ${it['entity_id'] ?? ''}'),
                        _kv('Provider ref', '${it['provider'] ?? ''} ${it['provider_ref'] ?? ''}'),
                        _kv('Details', it['details']),
                        _kv('Detected', it['created_at']),
                        if (it['resolution_note'] != null) _kv('Resolution', it['resolution_note']),
                        if (canAct && (it['status'] == 'open' || it['status'] == 'investigating'))
                          Wrap(spacing: AppSpace.sm, children: [
                            if (it['status'] == 'open')
                              OutlinedButton(onPressed: () => _update(context, ref, it, 'investigating'), child: const Text('Investigate')),
                            OutlinedButton(onPressed: () => _update(context, ref, it, 'resolved'), child: const Text('Resolve')),
                            TextButton(onPressed: () => _update(context, ref, it, 'dismissed'), child: const Text('Dismiss')),
                          ]),
                      ]),
                    ),
                  ),
              ]),
      ),
    ]);
  }

  Future<void> _update(BuildContext context, WidgetRef ref, Json it, String status) async {
    final note = status == 'investigating' ? '' : await _promptText(context, title: 'Resolution note', label: 'What was found and done?');
    if (note == null || !context.mounted) return;
    await _run(context, () => ref.read(adminRepositoryProvider).updateReconItem(it['id'] as String, status, note), 'Item updated');
    ref.invalidate(adminReconProvider);
  }
}

class AdminApprovalsScreen extends ConsumerWidget {
  const AdminApprovalsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final list = ref.watch(adminRequestsProvider);
    final me = ref.watch(currentUserProvider);
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      Row(children: [
        Expanded(child: Text('Financial requests (maker-checker)', style: AppText.headlineSm)),
        if (me?.isAdmin ?? false)
          FilledButton.icon(
            style: FilledButton.styleFrom(minimumSize: const Size(0, 44)),
            onPressed: () => showDialog<void>(context: context, builder: (_) => const _NewRequestDialog()).then((_) => ref.invalidate(adminRequestsProvider)),
            icon: const Icon(Icons.add),
            label: const Text('New request'),
          ),
      ]),
      const SizedBox(height: AppSpace.sm),
      Text('Adjustments and reversals post balanced ledger journals only after a second administrator approves.',
          style: AppText.bodySm.copyWith(color: AppColors.muted)),
      const SizedBox(height: AppSpace.md),
      list.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(adminRequestsProvider)),
        data: (rows) => rows.isEmpty
            ? const EmptyView(message: 'No financial requests.')
            : Column(children: [
                for (final f in rows)
                  Padding(
                    padding: const EdgeInsets.only(bottom: AppSpace.sm),
                    child: AppCard(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Row(children: [
                          Expanded(child: Text('${f['kind']}'.replaceAll('_', ' '), style: AppText.titleMd)),
                          Chip(label: Text('${f['status']}')),
                        ]),
                        _kv('Amount', f['amount'] == null ? null : '${f['currency']} ${f['amount']}'),
                        _kv('Wallet / target', f['wallet_id'] ?? f['target_id']),
                        _kv('Reason', f['reason_code']),
                        _kv('Memo', f['memo']),
                        _kv('Requested by', f['requested_by']),
                        if (f['failure_reason'] != null) _kv('Failure', f['failure_reason']),
                        if (f['status'] == 'pending_approval' && (me?.isAdmin ?? false))
                          f['requested_by'] == me!.id
                              ? Text('Awaiting another administrator', style: AppText.labelMd.copyWith(color: AppColors.pending))
                              : Wrap(spacing: AppSpace.sm, children: [
                                  FilledButton(
                                    style: FilledButton.styleFrom(minimumSize: const Size(0, 40)),
                                    onPressed: () => _decide(context, ref, f, true),
                                    child: const Text('Approve & post'),
                                  ),
                                  OutlinedButton(style: OutlinedButton.styleFrom(minimumSize: const Size(0, 40)), onPressed: () => _decide(context, ref, f, false), child: const Text('Reject')),
                                ]),
                      ]),
                    ),
                  ),
              ]),
      ),
    ]);
  }

  Future<void> _decide(BuildContext context, WidgetRef ref, Json f, bool approve) async {
    final note = await _promptText(context, title: approve ? 'Approve request' : 'Reject request', label: 'Decision note', minLength: 3);
    if (note == null || !context.mounted) return;
    await _run(context, () => ref.read(adminRepositoryProvider).decide(f['id'] as String, approve: approve, note: note), approve ? 'Posted' : 'Rejected');
    ref.invalidate(adminRequestsProvider);
  }
}

class _NewRequestDialog extends ConsumerStatefulWidget {
  const _NewRequestDialog();
  @override
  ConsumerState<_NewRequestDialog> createState() => _NewRequestDialogState();
}

class _NewRequestDialogState extends ConsumerState<_NewRequestDialog> {
  String _kind = 'adjustment_credit';
  String _reason = 'operational_error';
  final _target = TextEditingController();
  final _amount = TextEditingController();
  final _memo = TextEditingController();
  String? _error;

  bool get _adjustment => _kind.startsWith('adjustment');

  @override
  Widget build(BuildContext context) => AlertDialog(
        title: const Text('New financial request'),
        content: SizedBox(
          width: 420,
          child: SingleChildScrollView(
            child: Column(mainAxisSize: MainAxisSize.min, children: [
              DropdownButtonFormField<String>(
                initialValue: _kind,
                decoration: const InputDecoration(labelText: 'Kind'),
                items: const ['adjustment_credit', 'adjustment_debit', 'transfer_reversal', 'deposit_reversal', 'withdrawal_reversal']
                    .map((k) => DropdownMenuItem(value: k, child: Text(k.replaceAll('_', ' '))))
                    .toList(),
                onChanged: (v) => setState(() => _kind = v!),
              ),
              const SizedBox(height: AppSpace.sm),
              TextField(controller: _target, decoration: InputDecoration(labelText: _adjustment ? 'Wallet ID' : 'Transaction ID')),
              if (_adjustment) ...[
                const SizedBox(height: AppSpace.sm),
                TextField(controller: _amount, keyboardType: const TextInputType.numberWithOptions(decimal: true), decoration: const InputDecoration(labelText: 'Amount (KES)')),
              ],
              const SizedBox(height: AppSpace.sm),
              DropdownButtonFormField<String>(
                initialValue: _reason,
                decoration: const InputDecoration(labelText: 'Reason code'),
                items: const ['provider_reversal', 'customer_dispute', 'operational_error', 'fraud', 'goodwill', 'reconciliation_correction']
                    .map((k) => DropdownMenuItem(value: k, child: Text(k.replaceAll('_', ' '))))
                    .toList(),
                onChanged: (v) => setState(() => _reason = v!),
              ),
              const SizedBox(height: AppSpace.sm),
              TextField(controller: _memo, maxLines: 3, decoration: const InputDecoration(labelText: 'Memo (min 10 characters)')),
              if (_error != null) Padding(padding: const EdgeInsets.only(top: AppSpace.sm), child: Text(_error!, style: AppText.bodySm.copyWith(color: AppColors.danger))),
            ]),
          ),
        ),
        actions: [
          TextButton(onPressed: () => Navigator.pop(context), child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(minimumSize: const Size(0, 44)),
            onPressed: () async {
              try {
                await ref.read(adminRepositoryProvider).createFinancialRequest({
                  'kind': _kind,
                  if (_adjustment) 'wallet_id': _target.text.trim() else 'target_id': _target.text.trim(),
                  if (_adjustment) 'amount': _amount.text.trim(),
                  'reason_code': _reason,
                  'memo': _memo.text.trim(),
                });
                if (context.mounted) Navigator.pop(context);
              } on ApiException catch (e) {
                setState(() => _error = e.fields.isEmpty ? e.message : e.fields.values.join('; '));
              }
            },
            child: const Text('Submit for approval'),
          ),
        ],
      );
}

class AdminUsersScreen extends ConsumerWidget {
  const AdminUsersScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final users = ref.watch(adminUsersProvider);
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      TextField(
        decoration: const InputDecoration(prefixIcon: Icon(Icons.search), hintText: 'Search phone, name, email or account number'),
        onSubmitted: (v) => ref.read(adminUserQueryProvider.notifier).state = v.trim(),
      ),
      const SizedBox(height: AppSpace.md),
      users.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e),
        data: (rows) => Column(children: [
          for (final u in rows)
            Padding(
              padding: const EdgeInsets.only(bottom: AppSpace.sm),
              child: AppCard(
                onTap: () => context.go('/admin/users/${u['id']}'),
                child: Row(children: [
                  CircleAvatar(child: Text('${u['full_name']}'.substring(0, 1))),
                  const SizedBox(width: AppSpace.md),
                  Expanded(
                    child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                      Text('${u['full_name']}', style: AppText.titleMd),
                      Text('${u['phone']} • ${u['account_number'] ?? ''}', style: AppText.bodySm.copyWith(color: AppColors.muted)),
                    ]),
                  ),
                  Chip(label: Text('${u['status']}')),
                ]),
              ),
            ),
        ]),
      ),
    ]);
  }
}

class AdminUserDetailScreen extends ConsumerWidget {
  const AdminUserDetailScreen({super.key, required this.id});
  final String id;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final d = ref.watch(adminUserProvider(id));
    final canAct = ref.watch(currentUserProvider)?.isAdmin ?? false;
    return d.when(
      loading: () => const LoadingView(),
      error: (e, _) => ErrorView(error: e),
      data: (j) {
        final u = (j['user'] as Map).cast<String, dynamic>();
        final wallets = (j['wallets'] as List? ?? []).cast<Map>();
        return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
          AppCard(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text('${u['full_name']}', style: AppText.headlineSm),
              _kv('Phone', u['phone']),
              _kv('Email', u['email']),
              _kv('Status', '${u['status']}${u['status_reason'] != null ? ' (${u['status_reason']})' : ''}'),
              _kv('KYC tier', u['kyc_tier']),
              _kv('Role', u['role']),
              _kv('Phone verified', u['phone_verified_at']),
              _kv('Created', u['created_at']),
              if (canAct)
                Wrap(spacing: AppSpace.sm, children: [
                  for (final s in const ['active', 'restricted', 'suspended'])
                    if (s != u['status'])
                      OutlinedButton(
                        onPressed: () async {
                          final reason = await _promptText(context, title: 'Set status: $s', label: 'Reason', minLength: 5);
                          if (reason == null || !context.mounted) return;
                          await _run(context, () => ref.read(adminRepositoryProvider).setUserStatus(id, s, reason), 'Status updated');
                          ref.invalidate(adminUserProvider(id));
                        },
                        child: Text(s == 'active' ? 'Reactivate' : s == 'restricted' ? 'Restrict' : 'Suspend'),
                      ),
                ]),
            ]),
          ),
          const SizedBox(height: AppSpace.md),
          for (final w in wallets)
            AppCard(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text('Wallet ${w['account_number']} (${w['currency']})', style: AppText.titleMd),
                _kv('Wallet ID', w['id']),
                _kv('Available (projection)', _m(w['available'])),
                _kv('Available (derived from entries)', _m(w['derived_available'])),
                _kv('Held', _m(w['held'])),
                if (w['available'] != w['derived_available'])
                  Text('Projection mismatch - run reconciliation', style: AppText.labelLg.copyWith(color: AppColors.danger)),
              ]),
            ),
        ]);
      },
    );
  }
}

class AdminAuditScreen extends ConsumerWidget {
  const AdminAuditScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final events = ref.watch(adminAuditProvider);
    return ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
      TextField(
        decoration: const InputDecoration(prefixIcon: Icon(Icons.filter_list), hintText: 'Filter by exact action, e.g. admin.financial_request_posted'),
        onSubmitted: (v) => ref.read(adminAuditActionProvider.notifier).state = v.trim(),
      ),
      const SizedBox(height: AppSpace.md),
      events.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e),
        data: (rows) => Column(children: [
          for (final e in rows)
            Padding(
              padding: const EdgeInsets.only(bottom: AppSpace.sm),
              child: AppCard(
                child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                  Text('${e['action']}', style: AppText.labelLg),
                  _kv('When', e['created_at']),
                  _kv('Actor', '${e['actor_id'] ?? 'system'} (${e['actor_role'] ?? '-'})'),
                  _kv('Target', '${e['target_type'] ?? ''} ${e['target_id'] ?? ''}'),
                  _kv('Details', e['details']),
                  _kv('Request', '${e['request_id'] ?? ''} ${e['ip'] ?? ''}'),
                ]),
              ),
            ),
        ]),
      ),
    ]);
  }
}
