#include "models.h"

#include <QSignalSpy>
#include <QTest>

static TransferEntry transfer(const QString &path, const QString &state, quint64 done, quint64 total, const QString &dir = "upload")
{
    TransferEntry t;
    t.path = path;
    t.direction = dir;
    t.state = state;
    t.done = done;
    t.total = total;
    return t;
}

class TestModels : public QObject
{
    Q_OBJECT
private slots:
    void transferRoles()
    {
        TransferModel m;
        m.setTransfers({transfer("Docs/a.txt", "active", 1000, 4000), transfer("b.bin", "queued", 0, 0, "download")});
        QCOMPARE(m.rowCount(), 2);
        QCOMPARE(m.data(m.index(0), TransferModel::NameRole).toString(), QString("a.txt"));
        QCOMPARE(m.data(m.index(0), TransferModel::FolderRole).toString(), QString("Docs"));
        QCOMPARE(m.data(m.index(0), TransferModel::FractionRole).toDouble(), 0.25);
        QCOMPARE(m.data(m.index(0), TransferModel::StateTextRole).toString(), QString("Uploading"));
        QCOMPARE(m.data(m.index(0), TransferModel::IconNameRole).toString(), QString("text-plain"));
        QCOMPARE(m.data(m.index(1), TransferModel::FolderRole).toString(), QString());
        QVERIFY(m.data(m.index(1), TransferModel::FractionRole).toDouble() < 0);
        QCOMPARE(m.data(m.index(1), TransferModel::StateTextRole).toString(), QString("Queued"));
        QCOMPARE(m.data(m.index(5), TransferModel::NameRole), QVariant());
    }

    void transferProgressUpdatesInPlace()
    {
        TransferModel m;
        m.setTransfers({transfer("a", "active", 10, 100)});
        QSignalSpy reset(&m, &QAbstractItemModel::modelReset);
        QSignalSpy changed(&m, &QAbstractItemModel::dataChanged);
        m.setTransfers({transfer("a", "active", 50, 100)});
        QCOMPARE(reset.count(), 0);
        QCOMPARE(changed.count(), 1);
        QCOMPARE(m.data(m.index(0), TransferModel::FractionRole).toDouble(), 0.5);
        m.setTransfers({transfer("a", "active", 50, 100)}); // unchanged
        QCOMPARE(changed.count(), 1);
        m.setTransfers({transfer("a", "active", 50, 100), transfer("b", "queued", 0, 5)});
        QCOMPARE(reset.count(), 1);
        m.setTransfers({});
        QCOMPARE(m.rowCount(), 0);
    }

    void recentRoles()
    {
        const qint64 now = 1'800'000'000;
        RecentModel m;
        m.setClock([now] { return now; });
        m.setEntries({{"Docs/x.pdf", now - 120, "uploaded"}, {"y.txt", now - 5, "conflict"}, {"z", now - 5, "weird"}, {"New/n.pdf", now - 5, "added"}, {"e", now - 5, "error"}});
        QCOMPARE(m.rowCount(), 5);
        QCOMPARE(m.data(m.index(0), RecentModel::NameRole).toString(), QString("x.pdf"));
        QCOMPARE(m.data(m.index(0), RecentModel::FolderRole).toString(), QString("Docs"));
        QCOMPARE(m.data(m.index(0), RecentModel::ActionTextRole).toString(), QString("Uploaded"));
        QCOMPARE(m.data(m.index(0), RecentModel::TimeTextRole).toString(), QString("2 min ago"));
        QCOMPARE(m.data(m.index(0), RecentModel::IconNameRole).toString(), QString("application-pdf"));
        QCOMPARE(m.data(m.index(1), RecentModel::ActionIconRole).toString(), QString("dialog-warning"));
        QCOMPARE(m.data(m.index(1), RecentModel::TimeTextRole).toString(), QString("just now"));
        QCOMPARE(m.data(m.index(2), RecentModel::ActionTextRole).toString(), QString("Weird"));
        // unknown actions are not errors
        QCOMPARE(m.data(m.index(2), RecentModel::ActionIconRole).toString(), QString("dialog-information"));
        QCOMPARE(m.data(m.index(3), RecentModel::ActionTextRole).toString(), QString("Added"));
        QCOMPARE(m.data(m.index(3), RecentModel::ActionIconRole).toString(), QString("list-add"));
        QCOMPARE(m.data(m.index(4), RecentModel::ActionIconRole).toString(), QString("dialog-error"));
    }
};

QTEST_APPLESS_MAIN(TestModels)
#include "test_models.moc"
