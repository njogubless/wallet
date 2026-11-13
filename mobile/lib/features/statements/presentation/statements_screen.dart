import 'dart:io';

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path_provider/path_provider.dart';
import 'package:share_plus/share_plus.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/widgets/common.dart';
import '../data/statements_repository.dart';

final statementsProvider = FutureProvider.autoDispose<List<StatementInfo>>((ref) => ref.watch(statementsRepositoryProvider).list());

/// Saves a downloaded statement and opens the system share sheet. Replaceable
/// in tests.
typedef StatementSaver = Future<void> Function(String fileName, List<int> bytes);

final statementSaverProvider = Provider<StatementSaver>((_) => (name, bytes) async {
      if (kIsWeb) return;
      final dir = await getApplicationDocumentsDirectory();
      final file = File('${dir.path}/$name');
      await file.writeAsBytes(bytes, flush: true);
      await SharePlus.instance.share(ShareParams(files: [XFile(file.path)]));
    });

class StatementsScreen extends ConsumerStatefulWidget {
  const StatementsScreen({super.key});
  @override
  ConsumerState<StatementsScreen> createState() => _StatementsScreenState();
}

class _StatementsScreenState extends ConsumerState<StatementsScreen> {
  int _range = 30;
  String _format = 'pdf';
  bool _busy = false;

  Future<void> _generate() async {
    setState(() => _busy = true);
    try {
      final now = toEat(DateTime.now());
      final to = DateTime(now.year, now.month, now.day);
      final from = to.subtract(Duration(days: _range - 1));
      final st = await ref.read(statementsRepositoryProvider).generate(from: from, to: to, format: _format);
      ref.invalidate(statementsProvider);
      await _download(st);
    } catch (e) {
      if (mounted) showSnack(context, errorMessage(e));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _download(StatementInfo st) async {
    final bytes = await ref.read(statementsRepositoryProvider).download(st.id);
    await ref.read(statementSaverProvider)('statement_${st.periodStart}_${st.periodEnd}.${st.format}', bytes);
    if (mounted) showSnack(context, 'Statement ready (${(bytes.length / 1024).ceil()} KB)');
  }

  @override
  Widget build(BuildContext context) {
    final list = ref.watch(statementsProvider);
    return Scaffold(
      appBar: AppBar(title: const Text('Statements')),
      body: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
        AppCard(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text('Generate statement', style: AppText.headlineSm),
            Text('Balances are derived from the double-entry ledger.', style: AppText.labelMd.copyWith(color: AppColors.secondary)),
            const SizedBox(height: AppSpace.md),
            Text('Date interval', style: AppText.labelLg.copyWith(color: AppColors.secondary)),
            const SizedBox(height: AppSpace.sm),
            SegmentedButton<int>(
              segments: const [ButtonSegment(value: 30, label: Text('Last 30 days')), ButtonSegment(value: 90, label: Text('Last 3 months'))],
              selected: {_range},
              onSelectionChanged: (s) => setState(() => _range = s.first),
            ),
            const SizedBox(height: AppSpace.md),
            Text('Format', style: AppText.labelLg.copyWith(color: AppColors.secondary)),
            const SizedBox(height: AppSpace.sm),
            SegmentedButton<String>(
              segments: const [
                ButtonSegment(value: 'pdf', icon: Icon(Icons.picture_as_pdf_outlined), label: Text('PDF')),
                ButtonSegment(value: 'csv', icon: Icon(Icons.table_chart_outlined), label: Text('Ledger CSV')),
              ],
              selected: {_format},
              onSelectionChanged: (s) => setState(() => _format = s.first),
            ),
            const SizedBox(height: AppSpace.lg),
            FilledButton.icon(
              key: const Key('statements.generate'),
              onPressed: _busy ? null : _generate,
              icon: const Icon(Icons.download),
              label: Text(_busy ? 'Preparing…' : 'Download statement'),
            ),
          ]),
        ),
        const SizedBox(height: AppSpace.lg),
        const SectionLabel('Previous statements'),
        list.when(
          loading: () => const LoadingView(),
          error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(statementsProvider)),
          data: (items) => items.isEmpty
              ? const EmptyView(message: 'No statements yet.')
              : Column(children: [
                  for (final s in items)
                    Padding(
                      padding: const EdgeInsets.only(bottom: AppSpace.sm),
                      child: AppCard(
                        onTap: () => _download(s),
                        child: Row(children: [
                          Icon(s.format == 'pdf' ? Icons.picture_as_pdf_outlined : Icons.table_chart_outlined, color: AppColors.primary),
                          const SizedBox(width: AppSpace.md),
                          Expanded(
                            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                              Text('${s.periodStart} → ${s.periodEnd}', style: AppText.titleMd),
                              Text('Generated ${formatDateTime(s.createdAt)}', style: AppText.bodySm.copyWith(color: AppColors.muted)),
                            ]),
                          ),
                          const Icon(Icons.download_outlined),
                        ]),
                      ),
                    ),
                ]),
        ),
      ]),
    );
  }
}
