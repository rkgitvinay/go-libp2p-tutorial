WITH customer_orders AS (
    SELECT
        c.id AS customer_id,
        c.first_name AS "Name",
        c.phone AS "Phone Number",
        c.location AS "Location",
        COUNT(DISTINCT o.id) AS purchase_count, -- Count of distinct orders
        COALESCE(SUM(p.amount) / 100, 0) AS cumulative_lifetime_value, -- Lifetime value from the payment table
        MAX(o.created_at) AS last_purchase_date -- Last purchase date
    FROM
        customer c
    LEFT JOIN
        "order" o ON o.customer_id = c.id -- Orders linked to customers
    LEFT JOIN
        cart ca ON ca.id = o.cart_id -- Cart linked to the order
    LEFT JOIN
        payment p ON p.cart_id = ca.id -- Payment linked to the cart
    WHERE
        o.id IS NOT NULL -- Ensure only customers with at least one order are included
    GROUP BY
        c.id
),
last_purchase_items AS (
    SELECT
        o.customer_id AS customer_id,
        STRING_AGG(DISTINCT li.title, ', ') AS last_purchased_items
    FROM
        "order" o
    LEFT JOIN
        cart ca ON ca.id = o.cart_id
    LEFT JOIN
        line_item li ON li.cart_id = ca.id
    WHERE
        o.created_at = (
            SELECT MAX(o2.created_at)
            FROM "order" o2
            WHERE o2.customer_id = o.customer_id
        )
    GROUP BY
        o.customer_id
)
SELECT
    co."Name",
    co."Phone Number",
    co."Location",
    (co.purchase_count > 2) AS "isRepeatCustomer", -- Check for repeat customer
    co.purchase_count AS "Purchase Count",
    co.cumulative_lifetime_value AS "Cumulative Lifetime Value", -- Updated to use the payment table
    co.last_purchase_date AS "Last Purchase Date",
    lpi.last_purchased_items AS "Last Purchased Items",
    co.last_purchase_date + INTERVAL '30 days' AS "Next Purchase Date",
    json_agg(
        json_build_object(
            'address_1', a.address_1,
            'address_2', a.address_2,
            'city', a.city
        )
    ) AS "Addresses"
FROM
    customer_orders co
LEFT JOIN
    last_purchase_items lpi ON co.customer_id = lpi.customer_id
LEFT JOIN
    address a ON a.customer_id = co.customer_id
GROUP BY
    co.customer_id, co."Name", co."Phone Number", co."Location", co.purchase_count, co.cumulative_lifetime_value, co.last_purchase_date, lpi.last_purchased_items;
