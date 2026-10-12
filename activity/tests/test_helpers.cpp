#include "helpers.h"

#include <QDateTime>
#include <QTest>

class TestHelpers : public QObject
{
    Q_OBJECT
private slots:
    void splitPath_data()
    {
        QTest::addColumn<QString>("path");
        QTest::addColumn<QString>("dir");
        QTest::addColumn<QString>("name");
        QTest::newRow("top level") << "a.txt" << "" << "a.txt";
        QTest::newRow("nested") << "Docs/2024/report.pdf" << "Docs/2024" << "report.pdf";
        QTest::newRow("trailing slash") << "Docs/sub/" << "Docs" << "sub";
        QTest::newRow("empty") << "" << "" << "";
    }
    void splitPath()
    {
        QFETCH(QString, path);
        QFETCH(QString, dir);
        QFETCH(QString, name);
        const auto p = helpers::splitPath(path);
        QCOMPARE(p.dir, dir);
        QCOMPARE(p.name, name);
    }

    void relativeTime()
    {
        // local noon, so "hours ago" and "yesterday" do not depend on the time of day
        const qint64 now = QDateTime(QDate(2026, 6, 15), QTime(12, 0)).toSecsSinceEpoch();
        QCOMPARE(helpers::relativeTime(now, now), QString("just now"));
        QCOMPARE(helpers::relativeTime(now + 100, now), QString("just now")); // clock skew
        QCOMPARE(helpers::relativeTime(now - 120, now), QString("2 min ago"));
        QCOMPARE(helpers::relativeTime(now - 3600, now), QString("1 hour ago"));
        QCOMPARE(helpers::relativeTime(now - 3 * 3600, now), QString("3 hours ago"));
        QCOMPARE(helpers::relativeTime(now - 18 * 3600, now), QString("yesterday"));
        QCOMPARE(helpers::relativeTime(now - 3 * 86400, now), QString("3 days ago"));
        // older than a week: a locale formatted date, not a relative text
        QVERIFY(!helpers::relativeTime(now - 30 * 86400, now).contains("ago"));
    }

    void progressText()
    {
        QCOMPARE(helpers::progressText(1200000, 4500000), QString("1.2 MB of 4.5 MB"));
        QCOMPARE(helpers::progressText(1200000, 0), QString("1.2 MB"));
    }

    void quotaText()
    {
        QCOMPARE(helpers::quotaText(0, 0), QString());
        QCOMPARE(helpers::quotaText(1200000000, 5000000000), QString("Used 1.2 GB of 5.0 GB"));
    }

    void instanceLabel()
    {
        QCOMPARE(helpers::instanceLabel("me@example.com", "/home/me/OneDrive"), QString("me@example.com (OneDrive)"));
        QCOMPARE(helpers::instanceLabel("", "/home/me/OneDrive"), QString("OneDrive"));
    }
};

QTEST_APPLESS_MAIN(TestHelpers)
#include "test_helpers.moc"
